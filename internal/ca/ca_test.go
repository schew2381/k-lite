package ca_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schew2381/k-lite/internal/ca"
)

func testCA(t *testing.T) *ca.CA {
	t.Helper()
	c, err := ca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return c
}

func TestLoadOrCreateIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c1, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c2, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(c1.CertPEM, c2.CertPEM) {
		t.Error("reload returned a different cert")
	}
	if !c1.Key.Equal(c2.Key) {
		t.Error("reload returned a different key")
	}
	for _, name := range []string{"ca.crt", "ca.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, perm)
		}
	}
	if !c1.Cert.IsCA {
		t.Error("cert is not a CA")
	}
	wantExpiry := time.Now().AddDate(10, 0, 0)
	if d := c1.Cert.NotAfter.Sub(wantExpiry).Abs(); d > 24*time.Hour {
		t.Errorf("NotAfter = %v, want ~%v", c1.Cert.NotAfter, wantExpiry)
	}
}

func TestLoadOrCreatePartialState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), "partial ca state") {
		t.Errorf("want partial state error, got %v", err)
	}
}

func TestLoadOrCreateRejectsLooseKeyPerms(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "ca.key")
	for _, perm := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(keyPath, perm); err != nil {
			t.Fatal(err)
		}
		if _, err := ca.LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), "group- or world-accessible") {
			t.Errorf("mode %04o: want permission error, got %v", perm, err)
		}
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Errorf("reload after chmod 600: %v", err)
	}
}

func appendToFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// Trailing whitespace in ca.crt loads fine and CertPEM stays canonical, so a
// token minted before the edit still matches the bootstrap capture.
func TestLoadOrCreateCanonicalizesCertPEM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c1, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendToFile(t, filepath.Join(dir, "ca.crt"), []byte("\n\n  \n"))
	c2, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("reload with trailing whitespace: %v", err)
	}
	if !bytes.Equal(c1.CertPEM, c2.CertPEM) {
		t.Error("CertPEM not canonical after whitespace edit")
	}
}

func TestLoadOrCreateRejectsTrailingPEMData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
		data func(c *ca.CA) []byte
	}{
		{"garbage after cert", "ca.crt", func(*ca.CA) []byte { return []byte("garbage\n") }},
		{"second cert block", "ca.crt", func(c *ca.CA) []byte { return c.CertPEM }},
		{"garbage after key", "ca.key", func(*ca.CA) []byte { return []byte("garbage\n") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			c, err := ca.LoadOrCreate(dir)
			if err != nil {
				t.Fatal(err)
			}
			appendToFile(t, filepath.Join(dir, tt.file), tt.data(c))
			if _, err := ca.LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), "trailing data") {
				t.Errorf("want trailing data error, got %v", err)
			}
		})
	}
}

func TestLoadOrCreateMismatchedKey(t *testing.T) {
	t.Parallel()
	dir1, dir2 := t.TempDir(), t.TempDir()
	if _, err := ca.LoadOrCreate(dir1); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir2); err != nil {
		t.Fatal(err)
	}
	foreign, err := os.ReadFile(filepath.Join(dir2, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "ca.key"), foreign, 0o600); err != nil { // #nosec G703 -- t.TempDir paths
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir1); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("want key mismatch error, got %v", err)
	}
}

func TestServerCert(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	cert, err := c.ServerCert([]string{"127.0.0.1", "localhost", "host.docker.internal"})
	if err != nil {
		t.Fatalf("ServerCert: %v", err)
	}
	leaf := cert.Leaf
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Errorf("IP SANs = %v", leaf.IPAddresses)
	}
	wantDNS := []string{"localhost", "host.docker.internal"}
	if len(leaf.DNSNames) != 2 || leaf.DNSNames[0] != wantDNS[0] || leaf.DNSNames[1] != wantDNS[1] {
		t.Errorf("DNS SANs = %v, want %v", leaf.DNSNames, wantDNS)
	}
	if len(cert.Certificate) != 2 || !bytes.Equal(cert.Certificate[1], c.Cert.Raw) {
		t.Error("chain does not append the CA cert")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     c.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("verify against CA: %v", err)
	}
}

func TestSignNodeCSRRoundtrip(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	csrPEM, keyPEM, err := ca.NewNodeCSR("alice")
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}
	if block, _ := pem.Decode(keyPEM); block == nil || block.Type != "EC PRIVATE KEY" {
		t.Fatal("key PEM is not EC PRIVATE KEY")
	}
	certPEM, err := c.SignNodeCSR(csrPEM, "alice")
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse signed cert: %v", err)
	}
	node, ok := ca.NodeFromCert(cert)
	if !ok || node != "alice" {
		t.Errorf("NodeFromCert = %q, %v", node, ok)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != "klite:nodes" {
		t.Errorf("Organization = %v", cert.Subject.Organization)
	}
	// Both purposes, one identity: the node dials klited as a client and
	// serves its ingress listeners (ADR 0034).
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots:     c.Pool(),
			KeyUsages: []x509.ExtKeyUsage{usage},
		}); err != nil {
			t.Errorf("verify node cert for usage %v against CA: %v", usage, err)
		}
	}
	wantExpiry := time.Now().AddDate(1, 0, 0)
	if d := cert.NotAfter.Sub(wantExpiry).Abs(); d > 24*time.Hour {
		t.Errorf("NotAfter = %v, want ~%v", cert.NotAfter, wantExpiry)
	}
}

func TestSignNodeCSRIgnoresCSRNames(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	csrPEM, _, err := ca.NewNodeCSR("mallory")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := c.SignNodeCSR(csrPEM, "alice")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cn := cert.Subject.CommonName; cn != "klite:node:alice" {
		t.Errorf("CN = %q, want klite:node:alice", cn)
	}
}

func TestSignNodeCSRRejectsBadSignature(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	csrPEM, _, err := ca.NewNodeCSR("alice")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	der := bytes.Clone(block.Bytes)
	der[len(der)-1] ^= 0xFF
	if _, err := x509.ParseCertificateRequest(der); err != nil {
		t.Fatalf("tampered CSR must still parse so the signature check is what fails: %v", err)
	}
	tampered := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if _, err := c.SignNodeCSR(tampered, "alice"); err == nil || !strings.Contains(err.Error(), "csr signature") {
		t.Errorf("want csr signature error, got %v", err)
	}
}

func TestSignNodeCSRRejectsBadInput(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	csrPEM, _, err := ca.NewNodeCSR("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignNodeCSR(csrPEM, ""); err == nil {
		t.Error("want error for empty node name")
	}
	if _, err := c.SignNodeCSR(c.CertPEM, "alice"); err == nil {
		t.Error("want error for non-CSR PEM")
	}
	if _, err := c.SignNodeCSR([]byte("garbage"), "alice"); err == nil {
		t.Error("want error for garbage input")
	}
	oversize := append(bytes.Clone(csrPEM), bytes.Repeat([]byte{'A'}, 64*1024)...)
	if _, err := c.SignNodeCSR(oversize, "alice"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("want size cap error, got %v", err)
	}
	trailing := append(bytes.Clone(csrPEM), []byte("garbage")...)
	if _, err := c.SignNodeCSR(trailing, "alice"); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("want trailing data error, got %v", err)
	}
}

func TestSignNodeCSRRejectsWeakKey(t *testing.T) {
	t.Parallel()
	c := testCA(t)
	weak, err := rsa.GenerateKey(rand.Reader, 1024) // #nosec G403 -- deliberately weak, SignNodeCSR must refuse it
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "klite:node:mallory"},
	}, weak)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if _, err := c.SignNodeCSR(csrPEM, "mallory"); err == nil || !strings.Contains(err.Error(), "csr key") {
		t.Errorf("want weak key error, got %v", err)
	}
}

// A leaf must never claim validity past its issuer, or its final stretch
// would be unverifiable.
func TestLeafExpiryClampedToCA(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "short-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	c := &ca.CA{Cert: cert, Key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}

	serverCert, err := c.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !serverCert.Leaf.NotAfter.Equal(cert.NotAfter) {
		t.Errorf("server leaf NotAfter = %v, want CA's %v", serverCert.Leaf.NotAfter, cert.NotAfter)
	}

	csrPEM, _, err := ca.NewNodeCSR("alice")
	if err != nil {
		t.Fatal(err)
	}
	nodePEM, err := c.SignNodeCSR(csrPEM, "alice")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(nodePEM)
	nodeCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !nodeCert.NotAfter.Equal(cert.NotAfter) {
		t.Errorf("node cert NotAfter = %v, want CA's %v", nodeCert.NotAfter, cert.NotAfter)
	}
}

func TestNodeFromCert(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cn   string
		node string
		ok   bool
	}{
		{"node", "klite:node:alice", "alice", true},
		{"dotted", "klite:node:host.example", "host.example", true},
		{"empty name", "klite:node:", "", false},
		{"admin", "admin", "", false},
		{"ca cn", "klite-ca@123", "", false},
		{"empty cn", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert := &x509.Certificate{Subject: pkix.Name{CommonName: tt.cn}}
			node, ok := ca.NodeFromCert(cert)
			if node != tt.node || ok != tt.ok {
				t.Errorf("NodeFromCert(%q) = %q, %v; want %q, %v", tt.cn, node, ok, tt.node, tt.ok)
			}
		})
	}
}
