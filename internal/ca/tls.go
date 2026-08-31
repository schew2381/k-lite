package ca

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
)

// ServerTLS builds klited's single-listener config. Nodes present client
// certs, while the CLI and joining agents connect certless and authenticate
// per-RPC, so verification is if-given (research/grpc-go.md dual auth) and
// the deny-by-default interceptor carries the rest.
func ServerTLS(serverCert *tls.Certificate, caPool *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{*serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
}

// AgentTLS builds a joined agent's client config, with roots pinned to the
// cluster CA and the node cert presented.
func AgentTLS(caPool *x509.CertPool, clientCert *tls.Certificate) *tls.Config {
	return &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{*clientCert},
		MinVersion:   tls.VersionTLS12,
	}
}

// Bootstrap pins a join token's CA hash during the first, deliberately
// unverified dial — the k3s /cacerts trust-on-first-use pattern. The caller
// owns the dial: use TLSConfig (or wire VerifyPeerCertificate into your own
// config), handshake, then read the captured CA from CAPEM.
type Bootstrap struct {
	caHash string

	mu    sync.Mutex
	caPEM []byte
}

// NewBootstrap pins caHash, as parsed from the join token.
func NewBootstrap(caHash string) *Bootstrap {
	return &Bootstrap{caHash: caHash}
}

// TLSConfig replaces chain verification with the hash pin.
func (b *Bootstrap) TLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify:    true, // #nosec G402 -- verified against the token's pinned CA hash in VerifyPeerCertificate
		VerifyPeerCertificate: b.VerifyPeerCertificate,
		VerifyConnection:      b.verifyConnection,
		MinVersion:            tls.VersionTLS12,
	}
}

// verifyConnection re-runs the pin so a resumed session can't skip it.
//
//nolint:gocritic // the signature is fixed by tls.Config.VerifyConnection
func (b *Bootstrap) verifyConnection(cs tls.ConnectionState) error {
	raw := make([][]byte, len(cs.PeerCertificates))
	for i, cert := range cs.PeerCertificates {
		raw[i] = cert.Raw
	}
	return b.VerifyPeerCertificate(raw, nil)
}

// VerifyPeerCertificate hashes the presented root against the pin, verifies
// the leaf chains to it, and captures the CA PEM. The chain check matters:
// the CA cert is public, so a pin alone would pass anyone replaying it
// behind their own leaf. The join token rides this connection next.
func (b *Bootstrap) VerifyPeerCertificate(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("server presented no certificates")
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: rawCerts[len(rawCerts)-1]})
	if err := VerifyCAHash(caPEM, b.caHash); err != nil {
		return err
	}
	certs := make([]*x509.Certificate, len(rawCerts))
	for i, raw := range rawCerts {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parse peer certificate %d: %w", i, err)
		}
		certs[i] = cert
	}
	roots := x509.NewCertPool()
	roots.AddCert(certs[len(certs)-1])
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:max(1, len(certs)-1)] {
		intermediates.AddCert(cert)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("server cert does not chain to pinned ca: %w", err)
	}
	b.mu.Lock()
	b.caPEM = caPEM
	b.mu.Unlock()
	return nil
}

// CAPEM returns the CA captured by the last verified handshake, nil before
// one succeeds.
func (b *Bootstrap) CAPEM() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.caPEM)
}
