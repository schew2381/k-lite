// Package ca mints k-lite's cluster CA and everything hung off it: klited's
// serving cert, per-node client certs redeemed through join tokens, and the
// TLS configs both ends dial with (ADR 0013, research/join-auth.md).
package ca

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFile = "ca.crt"
	keyFile  = "ca.key"

	caYears   = 10
	leafYears = 1

	// backdate absorbs clock skew between klited and joining nodes.
	backdate = 5 * time.Minute

	// nodeCNPrefix marks node identity in cert subjects, the shape of
	// k3s's system:node:<name> convention.
	nodeCNPrefix = "klite:node:"
	nodeOrg      = "klite:nodes"

	pemTypeCert = "CERTIFICATE"
	pemTypeCSR  = "CERTIFICATE REQUEST"
	pemTypeKey  = "EC PRIVATE KEY"

	// maxCSRLen caps join CSRs, which arrive from peers that hold only a
	// token. A real CSR is under 2 KB.
	maxCSRLen = 64 * 1024
)

// CA holds the cluster root certificate and its signing key.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

// LoadOrCreate reloads the CA from <dir>/ca.crt and ca.key, minting a
// 10-year self-signed ECDSA P-256 root first if neither file exists.
func LoadOrCreate(dir string) (*CA, error) {
	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)
	certExists, err := fileExists(certPath)
	if err != nil {
		return nil, err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return nil, err
	}
	switch {
	case certExists && keyExists:
		return load(certPath, keyPath)
	case certExists != keyExists:
		return nil, fmt.Errorf("partial ca state in %s: need both %s and %s or neither", dir, certFile, keyFile)
	default:
		return create(dir, certPath, keyPath)
	}
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func load(certPath, keyPath string) (*CA, error) {
	if err := checkKeyPerms(keyPath); err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	// CertPEM is re-encoded from the parsed block rather than kept as raw
	// file bytes: join tokens pin sha256(CertPEM), and the bootstrap
	// verifier hashes its own re-encoding of the presented cert. Hand-edited
	// whitespace in ca.crt must not break that equality.
	cert, canonPEM, err := parseCertPEMStrict(certPEM)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", certPath, err)
	}
	key, err := parseKeyPEMStrict(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", keyPath, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
	}
	return &CA{Cert: cert, Key: key, CertPEM: canonPEM}, nil
}

// checkKeyPerms refuses a signing key that anyone besides the owner can
// touch. create writes 0600, so a looser mode means someone changed it.
func checkKeyPerms(keyPath string) error {
	info, err := os.Stat(keyPath)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o, want 0600: the ca key must not be group- or world-accessible", keyPath, perm)
	}
	return nil
}

func parseKeyPEMStrict(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(keyPEM)
	if block == nil || block.Type != pemTypeKey {
		return nil, fmt.Errorf("not %s PEM", pemTypeKey)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("trailing data after %s block", pemTypeKey)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func create(dir, certPath, keyPath string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl, err := newTemplate(&pkix.Name{CommonName: fmt.Sprintf("klite-ca@%d", now.Unix())}, now, caYears)
	if err != nil {
		return nil, err
	}
	tmpl.IsCA = true
	tmpl.BasicConstraintsValid = true
	tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

func newTemplate(subject *pkix.Name, now time.Time, years int) (*x509.Certificate, error) {
	// Random 128-bit serials need no issuance state to stay collision-free.
	// The +1 shifts the range to [1, 2^128]: RFC 5280 forbids serial 0.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	serial.Add(serial, big.NewInt(1))
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      *subject,
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.AddDate(years, 0, 0),
	}, nil
}

// newLeafTemplate is newTemplate clamped to the CA's own expiry, since a leaf
// that outlives its issuer can never verify past that point anyway.
func (c *CA) newLeafTemplate(subject *pkix.Name) (*x509.Certificate, error) {
	tmpl, err := newTemplate(subject, time.Now(), leafYears)
	if err != nil {
		return nil, err
	}
	if tmpl.NotAfter.After(c.Cert.NotAfter) {
		tmpl.NotAfter = c.Cert.NotAfter
	}
	return tmpl, nil
}

// ServerCert issues a one-year serving cert for klited's listeners, with
// hosts split into IP and DNS SANs. The returned chain appends the CA cert
// so bootstrap dials can pin and capture it.
func (c *CA) ServerCert(hosts []string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl, err := c.newLeafTemplate(&pkix.Name{CommonName: "klited"})
	if err != nil {
		return nil, err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.Cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// SignNodeCSR signs a join CSR into a one-year node cert. The CSR is only
// proof of key possession — CheckSignature is required (the line k3s skips),
// and every name the CSR carries is ignored in favor of a CN the server
// stamps itself. The cert carries both TLS purposes: the node dials klited
// as a client and serves its Envoy's mTLS ingress listeners (ADR 0024) —
// BoringSSL enforces the purpose per role, so ClientAuth alone cannot serve.
func (c *CA) SignNodeCSR(csrPEM []byte, node string) ([]byte, error) {
	if node == "" {
		return nil, errors.New("empty node name")
	}
	if len(csrPEM) > maxCSRLen {
		return nil, fmt.Errorf("csr exceeds %d bytes", maxCSRLen)
	}
	block, rest := pem.Decode(csrPEM)
	if block == nil || block.Type != pemTypeCSR {
		return nil, fmt.Errorf("not %s PEM", pemTypeCSR)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("trailing data after %s block", pemTypeCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	if err := checkCSRKey(csr.PublicKey); err != nil {
		return nil, fmt.Errorf("csr key: %w", err)
	}
	subject := &pkix.Name{CommonName: nodeCNPrefix + node, Organization: []string{nodeOrg}}
	tmpl, err := c.newLeafTemplate(subject)
	if err != nil {
		return nil, err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, csr.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}), nil
}

// checkCSRKey refuses keys too weak to carry node identity. NewNodeCSR always
// generates P-256, so this only ever fires on a foreign client.
func checkCSRKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return fmt.Errorf("ecdsa curve %s is below 256 bits", k.Curve.Params().Name)
		}
		return nil
	case ed25519.PublicKey:
		return nil
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return fmt.Errorf("rsa key is %d bits, need at least 2048", k.N.BitLen())
		}
		return nil
	}
	return fmt.Errorf("unsupported key type %T", pub)
}

// NewNodeCSR generates a node's ECDSA P-256 key and a CSR for Bootstrap.Join.
// The CSR names the node only as a debugging courtesy that SignNodeCSR discards.
func NewNodeCSR(node string) (csrPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeCNPrefix + node}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeCSR, Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: keyDER})
	return csrPEM, keyPEM, nil
}

// NodeFromCert extracts the node name from a klite:node:<name> subject CN.
func NodeFromCert(cert *x509.Certificate) (string, bool) {
	node, ok := strings.CutPrefix(cert.Subject.CommonName, nodeCNPrefix)
	if !ok || node == "" {
		return "", false
	}
	return node, true
}

// Pool returns a cert pool holding only this CA.
func (c *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return pool
}

// PoolFromPEM builds a cert pool from a PEM bundle, typically the CA a
// bootstrap dial captured.
func PoolFromPEM(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no certificates in ca PEM")
	}
	return pool, nil
}

// parseCertPEMStrict parses exactly one certificate block and returns its
// canonical re-encoding alongside. Anything after the block besides
// whitespace is an error: a second certificate here would silently change
// which root the cluster trusts.
func parseCertPEMStrict(certPEM []byte) (*x509.Certificate, []byte, error) {
	block, rest := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCert {
		return nil, nil, fmt.Errorf("not %s PEM", pemTypeCert)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, nil, fmt.Errorf("trailing data after %s block", pemTypeCert)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: block.Bytes}), nil
}
