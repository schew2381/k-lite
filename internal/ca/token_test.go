package ca_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/schew2381/k-lite/internal/ca"
)

func TestTokenRoundtrip(t *testing.T) {
	t.Parallel()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")
	sum := sha256.Sum256(caPEM)
	wantHash := hex.EncodeToString(sum[:])

	tok := ca.MintToken(caPEM, "s3cret")
	if want := "K10" + wantHash + "::node:s3cret"; tok != want {
		t.Errorf("MintToken = %q, want %q", tok, want)
	}
	parsed, err := ca.ParseToken(tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if parsed.CAHash != wantHash || parsed.Secret != "s3cret" {
		t.Errorf("ParseToken = %+v", parsed)
	}
	if err := ca.VerifyCAHash(caPEM, parsed.CAHash); err != nil {
		t.Errorf("VerifyCAHash: %v", err)
	}
}

func TestParseTokenRejects(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("ab", 32)
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"wrong prefix", "K11" + hash + "::node:x"},
		{"no separator", "K10" + hash + ":node:x"},
		{"short hash", "K10" + hash[:62] + "::node:x"},
		{"long hash", "K10" + hash + "ab::node:x"},
		{"non-hex hash", "K10" + strings.Repeat("zz", 32) + "::node:x"},
		{"wrong user", "K10" + hash + "::user:x"},
		{"missing secret", "K10" + hash + "::node:"},
		{"oversize", "K10" + hash + "::node:" + strings.Repeat("x", 5000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ca.ParseToken(tt.token); err == nil {
				t.Errorf("ParseToken(%q) succeeded, want error", tt.token)
			}
		})
	}
}

func TestParseTokenSecretKeepsColons(t *testing.T) {
	t.Parallel()
	tok, err := ca.ParseToken(ca.MintToken([]byte("pem"), "a:b:c"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Secret != "a:b:c" {
		t.Errorf("Secret = %q, want a:b:c", tok.Secret)
	}
}

func TestVerifyCAHashRejects(t *testing.T) {
	t.Parallel()
	caPEM := []byte("some ca pem")
	sum := sha256.Sum256(caPEM)
	good := hex.EncodeToString(sum[:])

	tampered := []byte(good)
	if tampered[0] == 'a' {
		tampered[0] = 'b'
	} else {
		tampered[0] = 'a'
	}
	tests := []struct {
		name string
		pem  []byte
		hash string
	}{
		{"tampered hash", caPEM, string(tampered)},
		{"different ca", []byte("another ca pem"), good},
		{"non-hex", caPEM, strings.Repeat("zz", 32)},
		{"wrong length", caPEM, "abcd"},
		{"empty", caPEM, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ca.VerifyCAHash(tt.pem, tt.hash); err == nil {
				t.Error("VerifyCAHash succeeded, want error")
			}
		})
	}
}
