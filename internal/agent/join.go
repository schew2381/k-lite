package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/schew2381/k-lite/internal/ca"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// join.go is the client half of ADR 0013: trade a join token for a CA-signed
// node identity on first contact, persist it under <state>/<node>/tls, and
// present it on every dial after that (research/join-auth.md).

const (
	nodeKeyFile  = "node.key"
	nodeCertFile = "node.crt"
	caCertFile   = "ca.crt"

	joinCallTimeout = 10 * time.Second
	probeTimeout    = 5 * time.Second
)

// Identity is a node's TLS material, loaded from disk or freshly joined.
type Identity struct {
	// TLS pins the cluster CA and presents the node cert (ca.AgentTLS).
	TLS *tls.Config
	// Dir holds node.key, node.crt, and ca.crt; the infra pod bind-mounts
	// it read-only so Envoy dials xDS with the same identity.
	Dir string
}

// JoinConfig wires EnsureIdentity.
type JoinConfig struct {
	Node      string
	Endpoints []string
	// Token is a K10 join token from `klite node token` (CA hash + secret),
	// or the bare cluster secret for same-machine dev where LocalCAFile is
	// readable.
	Token string
	// StateDir overrides ~/.klite/agent as the root for per-node files.
	StateDir string
	// LocalCAFile verifies a bare-secret join. Empty means
	// $KLITE_CA, then ~/.klite/server/tls/ca.crt.
	LocalCAFile string
}

// errServerAuthMissing marks a pre-M9 identity: valid for dialing klited,
// unusable as the serving cert on the node's ingress listeners.
var errServerAuthMissing = errors.New("node certificate lacks the server usage")

// EnsureIdentity returns the node's TLS identity, reusing what a previous run
// persisted and joining with the token only when nothing usable exists, the
// cluster no longer honors it, or it predates the ingress serving usage.
func EnsureIdentity(ctx context.Context, cfg *JoinConfig) (*Identity, error) {
	dir, err := identityDir(cfg.StateDir, cfg.Node)
	if err != nil {
		return nil, err
	}
	ident, err := loadIdentity(dir, cfg.Node)
	if errors.Is(err, errServerAuthMissing) {
		if cfg.Token == "" {
			return nil, fmt.Errorf(
				"the identity in %s predates M9 ingress (%w) and no --token is set to re-join (mint one: klite node token)", dir, err)
		}
		slog.Warn("persisted identity cannot serve ingress listeners, re-joining with the token", "dir", dir)
		ident, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ident != nil {
		switch verdict := probeIdentity(ctx, cfg.Endpoints, cfg.Node, ident); verdict {
		case identityUsable:
			slog.Info("reusing persisted node identity", "dir", dir)
			return ident, nil
		case identityRejected:
			if cfg.Token == "" {
				return nil, fmt.Errorf("the cluster rejected the identity in %s and no --token is set to re-join", dir)
			}
			slog.Warn("cluster rejected the persisted identity, re-joining with the token", "dir", dir)
		}
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("no identity in %s and no --token to join with (mint one: klite node token)", dir)
	}
	if err := join(ctx, cfg, dir); err != nil {
		return nil, err
	}
	ident, err = loadIdentity(dir, cfg.Node)
	if err == nil && ident == nil {
		err = fmt.Errorf("join succeeded but %s holds no identity", dir)
	}
	return ident, err
}

func identityDir(stateDir, node string) (string, error) {
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		stateDir = filepath.Join(home, ".klite", "agent")
	}
	return filepath.Join(stateDir, node, "tls"), nil
}

// loadIdentity reads a persisted identity, returning nil when the directory
// holds none. Partial or foreign material is an error, never silently joined
// over.
func loadIdentity(dir, node string) (*Identity, error) {
	keyPath := filepath.Join(dir, nodeKeyFile)
	certPath := filepath.Join(dir, nodeCertFile)
	caPath := filepath.Join(dir, caCertFile)
	var missing []string
	for _, p := range []string{keyPath, certPath, caPath} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, filepath.Base(p))
		} else if err != nil {
			return nil, err
		}
	}
	if len(missing) == 3 {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s is missing %s: remove the directory to re-join", dir, strings.Join(missing, ", "))
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load node cert: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	if got, ok := ca.NodeFromCert(leaf); !ok || got != node {
		return nil, fmt.Errorf("%s holds a certificate for %q, not %q", dir, leaf.Subject.CommonName, node)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		// Pre-M9 identities dial fine but can't serve the ingress
		// listeners (ADR 0024), and Envoy mounts these exact files. A
		// token in hand turns this into a silent one-time re-join.
		return nil, errServerAuthMissing
	}
	caPEM, err := os.ReadFile(caPath) // #nosec G703 -- path is derived from our own state dir
	if err != nil {
		return nil, err
	}
	pool, err := ca.PoolFromPEM(caPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", caPath, err)
	}
	return &Identity{TLS: ca.AgentTLS(pool, &cert), Dir: dir}, nil
}

type identityVerdict int

const (
	identityUsable identityVerdict = iota
	identityRejected
)

// probeIdentity asks each endpoint whether the persisted identity still
// opens doors, via a certless-token-less Register that only a valid client
// cert can carry. Unreachable endpoints count as usable: a dead control
// plane is no reason to burn a good identity, and Run retries registration
// anyway.
func probeIdentity(ctx context.Context, endpoints []string, node string, ident *Identity) identityVerdict {
	for _, ep := range endpoints {
		err := registerOnce(ctx, ep, credentials.NewTLS(ident.TLS), &klitev1.RegisterRequest{Node: node}, probeTimeout)
		switch {
		case err == nil, status.Code(err) == codes.PermissionDenied:
			// PermissionDenied is "node not declared yet": the transport
			// accepted the cert, the YAML just hasn't been applied.
			return identityUsable
		case status.Code(err) == codes.Unauthenticated, isTLSReject(err):
			slog.Warn("identity probe rejected", "endpoint", ep, "err", err)
			return identityRejected
		default:
			slog.Debug("identity probe inconclusive", "endpoint", ep, "err", err)
		}
	}
	return identityUsable
}

// isTLSReject spots handshake-level certificate refusals, which gRPC folds
// into Unavailable alongside ordinary dead-endpoint errors.
func isTLSReject(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"tls:", "x509:", "certificate"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// join redeems the token for a signed identity and persists it. The K10 form
// pins the CA hash across a deliberately unverified first dial (the k3s
// /cacerts move); the bare-secret form verifies against a locally readable
// CA file instead.
func join(ctx context.Context, cfg *JoinConfig, dir string) error {
	csrPEM, keyPEM, err := ca.NewNodeCSR(cfg.Node)
	if err != nil {
		return err
	}
	secret, tlsCfg, pin, err := joinCreds(cfg)
	if err != nil {
		return err
	}
	req := &klitev1.RegisterRequest{Node: cfg.Node, ClusterToken: secret, CsrPem: csrPEM}
	resp, err := joinRegister(ctx, cfg.Endpoints, credentials.NewTLS(tlsCfg), req)
	if err != nil {
		return err
	}
	if len(resp.GetCertPem()) == 0 || len(resp.GetCaPem()) == 0 {
		return errors.New("server admitted the node but returned no certificate: it runs without a CA")
	}
	if pin != "" {
		if err := ca.VerifyCAHash(resp.GetCaPem(), pin); err != nil {
			return fmt.Errorf("returned ca: %w", err)
		}
	}
	if err := persistIdentity(dir, keyPEM, resp.GetCertPem(), resp.GetCaPem()); err != nil {
		return err
	}
	slog.Info("joined: node identity persisted", "node", cfg.Node, "dir", dir)
	return nil
}

// joinCreds builds the join dial's TLS config from the token form.
func joinCreds(cfg *JoinConfig) (secret string, tlsCfg *tls.Config, pin string, err error) {
	if tok, perr := ca.ParseToken(cfg.Token); perr == nil {
		return tok.Secret, ca.NewBootstrap(tok.CAHash).TLSConfig(), tok.CAHash, nil
	}
	caFile := cfg.LocalCAFile
	if caFile == "" {
		caFile = os.Getenv("KLITE_CA")
	}
	if caFile == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			caFile = filepath.Join(home, ".klite", "server", "tls", "ca.crt")
		}
	}
	caPEM, err := os.ReadFile(caFile) // #nosec G703 -- operator-supplied CA path
	if err != nil {
		return "", nil, "", fmt.Errorf("--token is a bare secret and no CA is readable at %q to verify the server; "+
			"mint a pinned token with `klite node token` instead: %w", caFile, err)
	}
	pool, err := ca.PoolFromPEM(caPEM)
	if err != nil {
		return "", nil, "", fmt.Errorf("%s: %w", caFile, err)
	}
	return cfg.Token, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, "", nil
}

// joinRegister walks the endpoints with backoff until one admits the node.
// Authentication and pin failures stop the loop outright: neither a wrong
// token nor a wrong cluster heals by retrying.
func joinRegister(ctx context.Context, endpoints []string, creds credentials.TransportCredentials, req *klitev1.RegisterRequest) (*klitev1.RegisterResponse, error) {
	backoff := time.Second
	for {
		for _, ep := range endpoints {
			resp, err := registerFull(ctx, ep, creds, req, joinCallTimeout)
			switch {
			case err == nil:
				return resp, nil
			case status.Code(err) == codes.Unauthenticated:
				return nil, fmt.Errorf("join rejected by %s: %s", ep, status.Convert(err).Message())
			case strings.Contains(err.Error(), "ca hash mismatch"):
				return nil, fmt.Errorf("server at %s does not match the token's pinned CA "+
					"(wrong cluster, or someone is impersonating it): %w", ep, err)
			case status.Code(err) == codes.PermissionDenied:
				slog.Warn("join pending, retrying", "endpoint", ep, "err", status.Convert(err).Message())
			default:
				slog.Warn("join attempt failed, retrying", "endpoint", ep, "err", err)
			}
		}
		if !sleep(ctx, backoff) {
			return nil, ctx.Err()
		}
		backoff = min(backoff*2, retryBackoffMax)
	}
}

func registerOnce(ctx context.Context, ep string, creds credentials.TransportCredentials, req *klitev1.RegisterRequest, timeout time.Duration) error {
	_, err := registerFull(ctx, ep, creds, req, timeout)
	return err
}

func registerFull(ctx context.Context, ep string, creds credentials.TransportCredentials, req *klitev1.RegisterRequest, timeout time.Duration) (*klitev1.RegisterResponse, error) {
	conn, err := grpc.NewClient(ep, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return klitev1.NewAgentServiceClient(conn).Register(cctx, req)
}

// persistIdentity writes the identity trio 0600 under a 0700 dir, the k3s
// on-disk shape.
func persistIdentity(dir string, keyPEM, certPEM, caPEM []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{nodeKeyFile, keyPEM},
		{nodeCertFile, certPEM},
		{caCertFile, caPEM},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
