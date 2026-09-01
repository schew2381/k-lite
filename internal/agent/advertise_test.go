package agent

import (
	"context"
	"testing"
)

const donorHosts = `127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
192.168.5.2	host.docker.internal
10.44.0.11	8b1c2f3a9d
`

func TestHostsLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		host   string
		wantIP string
		wantOK bool
	}{
		{"the docker host-gateway pin", "host.docker.internal", "192.168.5.2", true},
		{"case-insensitive match", "HOST.DOCKER.INTERNAL", "192.168.5.2", true},
		{"loopback lines never answer", "localhost", "", false},
		{"absent name", "example.com", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ip, ok := hostsLookup([]byte(donorHosts), tt.host)
			if ip != tt.wantIP || ok != tt.wantOK {
				t.Fatalf("hostsLookup(%q) = %q, %v; want %q, %v", tt.host, ip, ok, tt.wantIP, tt.wantOK)
			}
		})
	}
}

func TestLiteralAdvertiseIP(t *testing.T) {
	t.Parallel()
	if got := literalAdvertiseIP("10.10.17.5"); got != "10.10.17.5" {
		t.Fatalf("literal IP = %q, want accepted as-is", got)
	}
	if got := literalAdvertiseIP("127.0.0.1"); got != "" {
		t.Fatalf("loopback = %q, must be refused", got)
	}
	if got := literalAdvertiseIP("host.docker.internal"); got != "" {
		t.Fatalf("hostname = %q, must wait for resolution", got)
	}
}

// A hostname flag resolves against the donor's /etc/hosts once the donor
// exists, and the resolved IP rides the next status report.
func TestEnsureAdvertiseIPFromDonorHosts(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := New(&Config{Node: "node-1", Runtime: rt, AdvertiseAddress: "host.docker.internal"})
	if got := a.currentAdvertiseIP(); got != "" {
		t.Fatalf("advertise before resolution = %q, want empty", got)
	}

	a.ensureAdvertiseIP(context.Background()) // donor missing: stays pending
	if got := a.currentAdvertiseIP(); got != "" {
		t.Fatalf("advertise with no donor = %q, want still empty", got)
	}

	rt.hostsFile = []byte(donorHosts)
	a.ensureAdvertiseIP(context.Background())
	if got := a.currentAdvertiseIP(); got != "192.168.5.2" {
		t.Fatalf("advertise = %q, want the donor's host-gateway line", got)
	}
}

// An IP-literal flag needs no donor at all: Register already carries it.
func TestEnsureAdvertiseIPLiteral(t *testing.T) {
	t.Parallel()
	a := New(&Config{Node: "node-1", Runtime: newFakeRuntime(), AdvertiseAddress: "10.10.17.5"})
	if got := a.currentAdvertiseIP(); got != "10.10.17.5" {
		t.Fatalf("advertise = %q, want the literal flag value", got)
	}
}
