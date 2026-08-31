package ca_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/schew2381/k-lite/internal/ca"
)

func nodeTLSCert(t *testing.T, c *ca.CA, node string) *tls.Certificate {
	t.Helper()
	csrPEM, keyPEM, err := ca.NewNodeCSR(node)
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}
	certPEM, err := c.SignNodeCSR(csrPEM, node)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return &pair
}

func listen(t *testing.T, cfg *tls.Config) net.Listener {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

type peerInfo struct {
	node    string
	hasCert bool
	err     error
}

// acceptOne handshakes a single connection and reports what the server saw.
func acceptOne(ln net.Listener) peerInfo {
	conn, err := ln.Accept()
	if err != nil {
		return peerInfo{err: err}
	}
	defer conn.Close()
	tconn := conn.(*tls.Conn)
	_ = tconn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tconn.Handshake(); err != nil {
		return peerInfo{err: err}
	}
	state := tconn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return peerInfo{}
	}
	node, _ := ca.NodeFromCert(state.PeerCertificates[0])
	return peerInfo{node: node, hasCert: true}
}

func dial(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d := &tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func TestMTLSRoundtrip(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	serverCert, err := c.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	serverCfg := ca.ServerTLS(serverCert, c.Pool())
	if serverCfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Fatalf("ClientAuth = %v, want VerifyClientCertIfGiven", serverCfg.ClientAuth)
	}
	ln := listen(t, serverCfg)

	pool, err := ca.PoolFromPEM(c.CertPEM)
	if err != nil {
		t.Fatal(err)
	}

	// A joined node presents its client cert and the server resolves its name.
	seen := make(chan peerInfo, 1)
	go func() { seen <- acceptOne(ln) }()
	if err := dial(t, ln.Addr().String(), ca.AgentTLS(pool, nodeTLSCert(t, c, "alice"))); err != nil {
		t.Fatalf("node dial: %v", err)
	}
	got := <-seen
	if got.err != nil {
		t.Fatalf("server handshake: %v", got.err)
	}
	if !got.hasCert || got.node != "alice" {
		t.Errorf("server saw %+v, want node alice", got)
	}

	// A certless client (CLI, joining agent) is admitted; per-RPC auth
	// takes over from there.
	go func() { seen <- acceptOne(ln) }()
	if err := dial(t, ln.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("certless dial: %v", err)
	}
	got = <-seen
	if got.err != nil {
		t.Fatalf("server handshake: %v", got.err)
	}
	if got.hasCert {
		t.Errorf("server saw a cert from the certless client: %+v", got)
	}
}

func TestBootstrapPinsAndCapturesCA(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	serverCert, err := c.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ln := listen(t, ca.ServerTLS(serverCert, c.Pool()))

	tok, err := ca.ParseToken(ca.MintToken(c.CertPEM, "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	b := ca.NewBootstrap(tok.CAHash)
	if b.CAPEM() != nil {
		t.Error("CAPEM non-nil before handshake")
	}

	seen := make(chan peerInfo, 1)
	go func() { seen <- acceptOne(ln) }()
	if err := dial(t, ln.Addr().String(), b.TLSConfig()); err != nil {
		t.Fatalf("bootstrap dial: %v", err)
	}
	<-seen
	if !bytes.Equal(b.CAPEM(), c.CertPEM) {
		t.Error("captured CA does not match ca.crt")
	}
	if err := ca.VerifyCAHash(b.CAPEM(), tok.CAHash); err != nil {
		t.Errorf("captured CA fails the pin it just passed: %v", err)
	}
}

func TestBootstrapRejectsWrongCA(t *testing.T) {
	t.Parallel()
	real, other := testCA(t), testCA(t)
	serverCert, err := real.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ln := listen(t, ca.ServerTLS(serverCert, real.Pool()))

	tok, err := ca.ParseToken(ca.MintToken(other.CertPEM, "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	b := ca.NewBootstrap(tok.CAHash)
	seen := make(chan peerInfo, 1)
	go func() { seen <- acceptOne(ln) }()
	err = dial(t, ln.Addr().String(), b.TLSConfig())
	<-seen
	if err == nil || !strings.Contains(err.Error(), "ca hash mismatch") {
		t.Errorf("want ca hash mismatch, got %v", err)
	}
	if b.CAPEM() != nil {
		t.Error("CAPEM captured despite pin failure")
	}
}

// A rogue server can replay the public CA cert behind its own leaf; the pin
// matches, so the chain check has to be what rejects it.
func TestBootstrapRejectsForeignLeafWithPinnedCA(t *testing.T) {
	t.Parallel()
	real, evil := testCA(t), testCA(t)
	evilCert, err := evil.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	mitm := &tls.Certificate{
		Certificate: [][]byte{evilCert.Certificate[0], real.Cert.Raw},
		PrivateKey:  evilCert.PrivateKey,
	}
	ln := listen(t, &tls.Config{Certificates: []tls.Certificate{*mitm}, MinVersion: tls.VersionTLS12})

	tok, err := ca.ParseToken(ca.MintToken(real.CertPEM, "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	b := ca.NewBootstrap(tok.CAHash)
	seen := make(chan peerInfo, 1)
	go func() { seen <- acceptOne(ln) }()
	err = dial(t, ln.Addr().String(), b.TLSConfig())
	<-seen
	if err == nil || !strings.Contains(err.Error(), "does not chain") {
		t.Errorf("want chain error, got %v", err)
	}
	if b.CAPEM() != nil {
		t.Error("CAPEM captured despite chain failure")
	}
}
