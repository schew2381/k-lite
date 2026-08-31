package ca

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Join tokens carry the CA pin out-of-band, k3s's K10 format:
// K10<sha256-hex of ca.crt PEM>::node:<secret>.
const (
	tokenPrefix = "K10"
	tokenUser   = "node"
	caHashLen   = sha256.Size * 2
)

// Token is a parsed join token.
type Token struct {
	CAHash string
	Secret string
}

// MintToken formats a join token pinning sha256(caPEM) over secret.
func MintToken(caPEM []byte, secret string) string {
	sum := sha256.Sum256(caPEM)
	return tokenPrefix + hex.EncodeToString(sum[:]) + "::" + tokenUser + ":" + secret
}

// ParseToken splits a join token, validating shape only. VerifyCAHash checks
// the pin and the server checks the secret.
func ParseToken(s string) (Token, error) {
	rest, ok := strings.CutPrefix(s, tokenPrefix)
	if !ok {
		return Token{}, fmt.Errorf("token missing %s prefix", tokenPrefix)
	}
	hash, cred, ok := strings.Cut(rest, "::")
	if !ok {
		return Token{}, errors.New("token missing :: separator")
	}
	if len(hash) != caHashLen {
		return Token{}, fmt.Errorf("ca hash must be %d hex chars, got %d", caHashLen, len(hash))
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return Token{}, fmt.Errorf("ca hash: %w", err)
	}
	secret, ok := strings.CutPrefix(cred, tokenUser+":")
	if !ok || secret == "" {
		return Token{}, fmt.Errorf("token credential must be %s:<secret>", tokenUser)
	}
	return Token{CAHash: hash, Secret: secret}, nil
}

// VerifyCAHash compares sha256(caPEM) against a token's pinned hash in
// constant time.
func VerifyCAHash(caPEM []byte, hash string) error {
	want, err := hex.DecodeString(hash)
	if err != nil || len(want) != sha256.Size {
		return errors.New("pinned ca hash is not a sha256 hex digest")
	}
	sum := sha256.Sum256(caPEM)
	if subtle.ConstantTimeCompare(sum[:], want) != 1 {
		return errors.New("ca hash mismatch: server ca does not match join token")
	}
	return nil
}
