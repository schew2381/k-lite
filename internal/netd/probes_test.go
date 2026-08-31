package netd

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

type fakeDialer struct {
	mu sync.Mutex
	up map[string]bool
}

func (d *fakeDialer) set(addr string, up bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.up[addr] = up
}

func (d *fakeDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up[addr] {
		return nil, errors.New("connection refused")
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func probeCfg(t *testing.T, targets ...*klitev1.ProbeTarget) *netConfig {
	t.Helper()
	cfg, err := parseConfig(&klitev1.NetDesired{ProbeTargets: targets}, 1)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return cfg
}

func wantStates(t *testing.T, p *prober, want map[string]bool) {
	t.Helper()
	got := p.snapshot()
	if len(got) != len(want) {
		t.Fatalf("snapshot has %d states, want %d: %v", len(got), len(want), got)
	}
	for _, s := range got {
		ready, ok := want[s.GetInstance()]
		if !ok {
			t.Errorf("unexpected instance %q", s.GetInstance())
			continue
		}
		if s.GetReady() != ready {
			t.Errorf("instance %q ready = %v, want %v", s.GetInstance(), s.GetReady(), ready)
		}
	}
}

func TestProberStateTransitions(t *testing.T) {
	d := &fakeDialer{up: map[string]bool{"10.44.128.1:8080": true}}
	ptr := &atomic.Pointer[netConfig]{}
	ptr.Store(probeCfg(t,
		&klitev1.ProbeTarget{Instance: "b-1", Ip: "10.44.128.1", Port: 8080},
		&klitev1.ProbeTarget{Instance: "b-2", Ip: "10.44.128.2", Port: 8080},
	))
	p := newProber(ptr, d.dial)
	ctx := context.Background()

	p.sweep(ctx)
	wantStates(t, p, map[string]bool{"b-1": true, "b-2": false})

	// Flip both: only the latest result per instance is retained.
	d.set("10.44.128.1:8080", false)
	d.set("10.44.128.2:8080", true)
	p.sweep(ctx)
	wantStates(t, p, map[string]bool{"b-1": false, "b-2": true})

	// Dropping a target from config drops its state.
	ptr.Store(probeCfg(t, &klitev1.ProbeTarget{Instance: "b-2", Ip: "10.44.128.2", Port: 8080}))
	p.sweep(ctx)
	wantStates(t, p, map[string]bool{"b-2": true})

	// Snapshot echoes the target's ip and port.
	s := p.snapshot()[0]
	if s.GetIp() != "10.44.128.2" || s.GetPort() != 8080 {
		t.Errorf("snapshot = %v", s)
	}
}

func TestProberKickNowNeverBlocks(t *testing.T) {
	p := newProber(&atomic.Pointer[netConfig]{}, (&fakeDialer{up: map[string]bool{}}).dial)
	p.kickNow()
	p.kickNow()
	p.kickNow()
}
