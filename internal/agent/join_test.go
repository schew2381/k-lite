package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/schew2381/k-lite/internal/ca"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// joinServer is a minimal klited stand-in: real CA, real TLS listener with
// VerifyClientCertIfGiven, and a Register that mirrors the production
// contract (cert re-registration or token, CSR signing).
type joinServer struct {
	klitev1.UnimplementedAgentServiceServer
	authority *ca.CA
	secret    string
}

func (s *joinServer) Register(ctx context.Context, req *klitev1.RegisterRequest) (*klitev1.RegisterResponse, error) {
	if !s.certified(ctx, req.GetNode()) &&
		subtle.ConstantTimeCompare([]byte(req.GetClusterToken()), []byte(s.secret)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "bad cluster token")
	}
	resp := &klitev1.RegisterResponse{Net: &klitev1.NetBootstrap{NodeIndex: 1}}
	if len(req.GetCsrPem()) > 0 {
		certPEM, err := s.authority.SignNodeCSR(req.GetCsrPem(), req.GetNode())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "sign csr: %v", err)
		}
		resp.CertPem = certPEM
		resp.CaPem = s.authority.CertPEM
	}
	return resp, nil
}

func (s *joinServer) certified(ctx context.Context, node string) bool {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr.AuthInfo == nil {
		return false
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return false
	}
	got, ok := ca.NodeFromCert(tlsInfo.State.VerifiedChains[0][0])
	return ok && got == node
}

// startJoinServer runs the stand-in on a loopback listener and returns its
// address plus the CA it mints.
func startJoinServer(t *testing.T, secret string) (string, *ca.CA) {
	t.Helper()
	authority, err := ca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := authority.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(ca.ServerTLS(serverCert, authority.Pool()))))
	klitev1.RegisterAgentServiceServer(srv, &joinServer{authority: authority, secret: secret})
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), authority
}

func TestJoinPersistsAndReusesIdentity(t *testing.T) {
	t.Parallel()
	addr, authority := startJoinServer(t, "join-me")
	stateDir := t.TempDir()
	token := ca.MintToken(authority.CertPEM, "join-me")
	ctx := context.Background()

	ident, err := EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{addr}, Token: token, StateDir: stateDir})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	for _, f := range []string{"node.key", "node.crt", "ca.crt"} {
		info, err := os.Stat(filepath.Join(ident.Dir, f))
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s has mode %04o, want 0600", f, perm)
		}
	}

	// Second run: no token needed, the persisted identity carries it.
	reused, err := EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{addr}, StateDir: stateDir})
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if reused.Dir != ident.Dir {
		t.Fatalf("reuse landed in %s, joined in %s", reused.Dir, ident.Dir)
	}
	// And the identity actually opens the certified re-register path.
	if err := registerOnce(ctx, addr, credentials.NewTLS(reused.TLS), &klitev1.RegisterRequest{Node: "node-1"}, probeTimeout); err != nil {
		t.Fatalf("certified re-register: %v", err)
	}
}

func TestJoinRejectsTamperedPin(t *testing.T) {
	t.Parallel()
	addr, authority := startJoinServer(t, "join-me")
	token := ca.MintToken(authority.CertPEM, "join-me")
	// Flip one hash nibble without breaking the token shape.
	broken := []byte(token)
	if broken[3] == 'a' {
		broken[3] = 'b'
	} else {
		broken[3] = 'a'
	}
	_, err := EnsureIdentity(context.Background(), &JoinConfig{
		Node: "node-1", Endpoints: []string{addr}, Token: string(broken), StateDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "pinned CA") {
		t.Fatalf("tampered pin joined anyway: %v", err)
	}
}

func TestJoinFailsFastOnWrongSecret(t *testing.T) {
	t.Parallel()
	addr, authority := startJoinServer(t, "join-me")
	token := ca.MintToken(authority.CertPEM, "WRONG")
	_, err := EnsureIdentity(context.Background(), &JoinConfig{
		Node: "node-1", Endpoints: []string{addr}, Token: token, StateDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "join rejected") {
		t.Fatalf("wrong secret joined anyway: %v", err)
	}
}

func TestEnsureIdentityRejoinsWhenClusterForgotUs(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ctx := context.Background()

	oldAddr, oldCA := startJoinServer(t, "join-me")
	oldToken := ca.MintToken(oldCA.CertPEM, "join-me")
	if _, err := EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{oldAddr}, Token: oldToken, StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}

	// A different cluster (fresh CA) rejects the old cert at the handshake;
	// with a valid token for the new cluster the agent re-joins in place.
	newAddr, newCA := startJoinServer(t, "join-me")
	newToken := ca.MintToken(newCA.CertPEM, "join-me")
	ident, err := EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{newAddr}, Token: newToken, StateDir: stateDir})
	if err != nil {
		t.Fatalf("re-join: %v", err)
	}
	if err := registerOnce(ctx, newAddr, credentials.NewTLS(ident.TLS), &klitev1.RegisterRequest{Node: "node-1"}, probeTimeout); err != nil {
		t.Fatalf("re-joined identity unusable: %v", err)
	}

	// Without a token, the same rejection is fatal, never silent.
	_, err = EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{startJoinServerAddr(t)}, StateDir: stateDir})
	if err == nil {
		t.Fatal("rejected identity with no token should be an error")
	}
}

func startJoinServerAddr(t *testing.T) string {
	t.Helper()
	addr, _ := startJoinServer(t, "join-me")
	return addr
}

// writeLegacyIdentity persists a pre-M9 identity: CA-chained, correct CN,
// ClientAuth only — the exact shape SignNodeCSR produced before ingress.
func writeLegacyIdentity(t *testing.T, stateDir, node string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "legacy-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "klite:node:" + node},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, node, "tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]*pem.Block{
		"node.crt": {Type: "CERTIFICATE", Bytes: leafDER},
		"node.key": {Type: "EC PRIVATE KEY", Bytes: keyDER},
		"ca.crt":   {Type: "CERTIFICATE", Bytes: caDER},
	}
	for name, block := range files {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A persisted pre-M9 identity can dial but not serve ingress, so it re-joins
// when a token is in hand and fails with a pointed error when not.
func TestEnsureIdentityRejoinsPreM9Cert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stateDir := t.TempDir()
	writeLegacyIdentity(t, stateDir, "node-1")

	if _, err := loadIdentity(filepath.Join(stateDir, "node-1", "tls"), "node-1"); !errors.Is(err, errServerAuthMissing) {
		t.Fatalf("loadIdentity = %v, want errServerAuthMissing", err)
	}

	_, err := EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{"127.0.0.1:1"}, StateDir: stateDir})
	if err == nil || !strings.Contains(err.Error(), "predates M9") {
		t.Fatalf("tokenless err = %v, want the pointed pre-M9 message", err)
	}

	addr, authority := startJoinServer(t, "join-me")
	token := ca.MintToken(authority.CertPEM, "join-me")
	ident, err := EnsureIdentity(ctx, &JoinConfig{Node: "node-1", Endpoints: []string{addr}, Token: token, StateDir: stateDir})
	if err != nil {
		t.Fatalf("re-join over a pre-M9 identity: %v", err)
	}
	if _, err := loadIdentity(ident.Dir, "node-1"); err != nil {
		t.Fatalf("re-joined identity should now carry the server usage: %v", err)
	}
}

func TestLoadIdentityRefusesForeignNode(t *testing.T) {
	t.Parallel()
	addr, authority := startJoinServer(t, "join-me")
	stateDir := t.TempDir()
	token := ca.MintToken(authority.CertPEM, "join-me")
	ident, err := EnsureIdentity(context.Background(), &JoinConfig{Node: "node-1", Endpoints: []string{addr}, Token: token, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	// Same directory, different node name: never silently impersonate.
	if _, err := loadIdentity(ident.Dir, "node-2"); err == nil {
		t.Fatal("identity for node-1 loaded as node-2")
	}
}
