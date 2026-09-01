package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	"github.com/schew2381/k-lite/internal/ca"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const defaultServer = "127.0.0.1:7443"

// connCfg carries the root connection flags every command shares.
type connCfg struct {
	server   string
	insecure bool
}

type fileConfig struct {
	Endpoints []string `json:"endpoints"`
}

// serverFile resolves a file under klited's default state dir. The CLI reads
// (never writes) what klited minted there at first boot.
func serverFile(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home, ".klite", "server"}, parts...)...)
}

// adminToken resolves the bearer token: KLITE_TOKEN, then the token file
// klited mints at first boot. Empty means the server will turn the call away
// with its own explanation.
func adminToken() string {
	if t := os.Getenv("KLITE_TOKEN"); t != "" {
		return strings.TrimSpace(t)
	}
	if path := serverFile("token"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// endpoints resolves the server list: --server flag, then KLITE_SERVER, then ~/.klite/config, then the default.
func endpoints(flagValue string) []string {
	if flagValue != "" {
		return splitList(flagValue)
	}
	if env := os.Getenv("KLITE_SERVER"); env != "" {
		return splitList(env)
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".klite", "config")
		if b, err := os.ReadFile(path); err == nil {
			var c fileConfig
			if err := yaml.Unmarshal(b, &c); err != nil {
				fmt.Fprintf(os.Stderr, "warning: ignoring %s: %v\n", path, err)
			} else if len(c.Endpoints) > 0 {
				return c.Endpoints
			}
		}
	}
	return []string{defaultServer}
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// transportCreds builds the CLI's TLS credentials: roots pinned to the
// cluster CA from KLITE_CA or klited's state dir, or certificate-blind TLS
// under --insecure (klited has no plaintext listener, ADR 0013).
func (c *connCfg) transportCreds() (credentials.TransportCredentials, error) {
	if c.insecure {
		// #nosec G402 -- the --insecure escape hatch trades verification away explicitly
		return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}), nil
	}
	path := os.Getenv("KLITE_CA")
	if path == "" {
		path = serverFile("tls", "ca.crt")
	}
	caPEM, err := os.ReadFile(path) // #nosec G703 -- the CA path is the operator's own KLITE_CA input
	if err != nil {
		return nil, fmt.Errorf("cluster CA unreadable at %s (set KLITE_CA, or pass --insecure to skip verification): %w", path, err)
	}
	pool, err := ca.PoolFromPEM(caPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
}

// bearerCreds attaches the admin token to every call. Transport security is
// not required here so --insecure keeps working; the wire is TLS either way.
type bearerCreds string

func (b bearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (b bearerCreds) RequireTransportSecurity() bool { return false }

// callOpts is every per-connection option shared by dial and dialOne.
func (c *connCfg) callOpts() ([]grpc.DialOption, error) {
	creds, err := c.transportCreds()
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if tok := adminToken(); tok != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearerCreds(tok)))
	} else {
		fmt.Fprintln(os.Stderr, "warning: no admin token found (set KLITE_TOKEN or let klited mint ~/.klite/server/token)")
	}
	return opts, nil
}

// dial opens one lazy connection that round-robins over every endpoint, so any
// live klited answers (research/grpc-go.md). WaitForReady queues one-shot calls
// until a backend connects or the command deadline hits.
func dial(cfg *connCfg) (*grpc.ClientConn, klitev1.ClusterServiceClient, error) {
	eps := endpoints(cfg.server)
	addrs := make([]resolver.Address, 0, len(eps))
	for _, e := range eps {
		// ServerName pins TLS verification to the endpoint's own host;
		// without it the handshake would check the placeholder authority.
		addrs = append(addrs, resolver.Address{Addr: e, ServerName: hostOf(e)})
	}
	rb := manual.NewBuilderWithScheme("klite")
	rb.InitialState(resolver.State{Addresses: addrs})

	opts, err := cfg.callOpts()
	if err != nil {
		return nil, nil, err
	}
	opts = append(opts,
		grpc.WithResolvers(rb),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	)
	conn, err := grpc.NewClient("klite:///control-plane", opts...)
	if err != nil {
		return nil, nil, err
	}
	return conn, klitev1.NewClusterServiceClient(conn), nil
}

// dialOne targets a single endpoint, unlike dial's round-robin pool. Log
// streams need it because only the klited holding the target agent's command
// stream can serve them, so the caller walks endpoints itself. No
// WaitForReady here: a dead endpoint should fail fast so the walk moves on.
func dialOne(cfg *connCfg, addr string) (*grpc.ClientConn, klitev1.ClusterServiceClient, error) {
	opts, err := cfg.callOpts()
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, nil, err
	}
	return conn, klitev1.NewClusterServiceClient(conn), nil
}

// hostOf strips the port for TLS server-name checks, keeping the input when
// it has none.
func hostOf(ep string) string {
	if h, _, err := net.SplitHostPort(ep); err == nil && h != "" {
		return h
	}
	return ep
}

// rpcErr unwraps a gRPC status so users see the message, not the wire framing.
func rpcErr(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := status.FromError(err); ok {
		return fmt.Errorf("%s", s.Message())
	}
	return err
}
