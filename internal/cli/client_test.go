package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The endpoint list feeds both the round-robin dial and the logs walk, so
// the precedence (flag, then KLITE_SERVER, then ~/.klite/config, then the
// default) decides which klited replicas a command can fail over to.
func TestEndpointsPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KLITE_SERVER", "")

	if got := endpoints(""); !slices.Equal(got, []string{defaultServer}) {
		t.Fatalf("endpoints with nothing configured = %v, want the default", got)
	}

	dir := filepath.Join(home, ".klite")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "endpoints:\n  - 10.0.0.1:7443\n  - 10.0.0.2:7443\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(""); !slices.Equal(got, []string{"10.0.0.1:7443", "10.0.0.2:7443"}) {
		t.Fatalf("endpoints from the config file = %v", got)
	}

	t.Setenv("KLITE_SERVER", "10.1.1.1:7443, 10.1.1.2:7443")
	if got := endpoints(""); !slices.Equal(got, []string{"10.1.1.1:7443", "10.1.1.2:7443"}) {
		t.Fatalf("endpoints from KLITE_SERVER = %v", got)
	}
	if got := endpoints("10.2.2.2:7443"); !slices.Equal(got, []string{"10.2.2.2:7443"}) {
		t.Fatalf("endpoints from the flag = %v", got)
	}
}

// hostOf feeds the TLS ServerName for every endpoint, so a wrong split here
// would fail the handshake against a correctly issued serving cert.
func TestHostOf(t *testing.T) {
	t.Parallel()
	tests := []struct{ ep, want string }{
		{"10.0.0.1:7443", "10.0.0.1"},
		{"klited.example:7443", "klited.example"},
		{"[::1]:7443", "::1"},
		{"noport", "noport"},
	}
	for _, tt := range tests {
		if got := hostOf(tt.ep); got != tt.want {
			t.Errorf("hostOf(%q) = %q, want %q", tt.ep, got, tt.want)
		}
	}
}
