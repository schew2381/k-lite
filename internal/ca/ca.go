// Package ca mints k-lite's cluster CA and everything hung off it: klited's
// serving cert, per-node client certs redeemed through join tokens, and the
// TLS configs both ends dial with (ADR 0013, research/join-auth.md).
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", certPath, err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != pemTypeKey {
		return nil, fmt.Errorf("load %s: not %s PEM", keyPath, pemTypeKey)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", keyPath, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
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
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      *subject,
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.AddDate(years, 0, 0),
	}, nil
}

// ServerCert issues a one-year serving cert for klited's listeners, hosts
// split into IP and DNS SANs. The returned chain appends the CA cert so
// bootstrap dials can pin and capture it.
func (c *CA) ServerCert(hosts []string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl, err := newTemplate(&pkix.Name{CommonName: "klited"}, time.Now(), leafYears)
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

// SignNodeCSR signs a join CSR into a one-year client cert for node. The CSR
// is only proof of key possession — CheckSignature is required (the line k3s
// skips), and every name the CSR carries is ignored in favor of a CN the
// server stamps itself.
func (c *CA) SignNodeCSR(csrPEM []byte, node string) ([]byte, error) {
	if node == "" {
		return nil, errors.New("empty node name")
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != pemTypeCSR {
		return nil, fmt.Errorf("not %s PEM", pemTypeCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	subject := &pkix.Name{CommonName: nodeCNPrefix + node, Organization: []string{nodeOrg}}
	tmpl, err := newTemplate(subject, time.Now(), leafYears)
	if err != nil {
		return nil, err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, csr.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}), nil
}

// NewNodeCSR generates a node's ECDSA P-256 key and a CSR for Bootstrap.Join.
// The CSR names the node only as a debugging courtesy; SignNodeCSR discards it.
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

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCert {
		return nil, fmt.Errorf("not %s PEM", pemTypeCert)
	}
	return x509.ParseCertificate(block.Bytes)
}
