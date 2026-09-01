package server

import (
	"testing"
)

// NetBootstrap must relay --net-image untouched (ADR 0038): agents treat an
// empty value as "use the compiled-in default", so the server never invents
// one.
func TestNetBootstrapCarriesNetImage(t *testing.T) {
	t.Parallel()
	pinned := NewAgent(&AgentConfig{NetImage: "ghcr.io/schew2381/klite-net:v0.1.0"})
	if got := pinned.netBootstrap(3).GetNetImage(); got != "ghcr.io/schew2381/klite-net:v0.1.0" {
		t.Fatalf("net_image = %q, want the configured pin", got)
	}
	bare := NewAgent(&AgentConfig{})
	if got := bare.netBootstrap(3).GetNetImage(); got != "" {
		t.Fatalf("net_image = %q, want empty when unconfigured", got)
	}
}
