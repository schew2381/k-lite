package netd

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func testServer(bind func(string, []netip.Addr) (int, error)) *Server {
	if bind == nil {
		bind = func(_ string, want []netip.Addr) (int, error) { return len(want), nil }
	}
	return New(Options{
		DNSListen: ":0",
		Upstream:  "127.0.0.1:1",
		Iface:     "test0",
		dial:      (&fakeDialer{up: map[string]bool{}}).dial,
		bindVIPs:  bind,
	})
}

func desired(vipOctet int) *klitev1.NetDesired {
	return &klitev1.NetDesired{
		Services: []*klitev1.ServiceVIP{
			{Service: "b", Vip: fmt.Sprintf("10.44.64.%d", vipOctet), Port: 8080},
		},
		ProbeTargets: []*klitev1.ProbeTarget{
			{Instance: fmt.Sprintf("b-%d", vipOctet), Ip: "10.44.128.9", Port: 8080},
		},
	}
}

// Each pushed config pairs VIP 10.44.64.N with probe instance b-N. Readers
// racing ApplyConfig must only ever observe matched pairs: a torn config
// would mix octets. Run with -race.
func TestApplyConfigSwapAtomicity(t *testing.T) {
	srv := testServer(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				octet := w*50 + i + 1
				if _, err := srv.ApplyConfig(ctx, &klitev1.ApplyConfigRequest{Net: desired(octet)}); err != nil {
					t.Errorf("ApplyConfig: %v", err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	for {
		cfg := srv.cfg.Load()
		if len(cfg.vips) > 0 {
			vip := cfg.vips["b"]
			wantInstance := fmt.Sprintf("b-%d", vip.As4()[3])
			if cfg.targets[0].instance != wantInstance {
				t.Fatalf("torn config: vip %s with target %s", vip, cfg.targets[0].instance)
			}
		}
		select {
		case <-done:
			if got := srv.serial.Load(); got != 200 {
				t.Fatalf("serial counter = %d, want 200 after 200 applies", got)
			}
			// Pushes are serialized, so the stored config must be the
			// one that drew the final serial. An older racing push
			// sticking around would mean stale DNS the agent believes
			// it replaced.
			if got := srv.cfg.Load().serial; got != 200 {
				t.Fatalf("stored config serial = %d, want 200 (stale push won the race)", got)
			}
			return
		default:
		}
	}
}

func TestApplyConfigRejectsBadVIPs(t *testing.T) {
	srv := testServer(nil)
	ctx := context.Background()
	if _, err := srv.ApplyConfig(ctx, &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	for _, tt := range []struct {
		name string
		vip  string
	}{
		{name: "not an ip", vip: "nope"},
		{name: "outside pool", vip: "10.44.0.5"},
		{name: "ipv6", vip: "::1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := &klitev1.ApplyConfigRequest{Net: &klitev1.NetDesired{
				Services: []*klitev1.ServiceVIP{{Service: "b", Vip: tt.vip}},
			}}
			_, err := srv.ApplyConfig(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
		})
	}

	// The rejected pushes must not have replaced the good config.
	if got := srv.cfg.Load().vips["b"].String(); got != "10.44.64.9" {
		t.Fatalf("config after rejects = %s, want 10.44.64.9", got)
	}
}

func TestParseConfigServiceNames(t *testing.T) {
	svc := func(names ...string) *klitev1.NetDesired {
		net := &klitev1.NetDesired{}
		for i, n := range names {
			net.Services = append(net.Services, &klitev1.ServiceVIP{
				Service: n, Vip: fmt.Sprintf("10.44.64.%d", i+1), Port: 80,
			})
		}
		return net
	}
	tests := []struct {
		name    string
		net     *klitev1.NetDesired
		wantErr bool
	}{
		{name: "plain label", net: svc("b")},
		{name: "dashes inside", net: svc("api-v2")},
		{name: "uppercase folds", net: svc("MixedCase")},
		{name: "63 chars fits", net: svc(strings.Repeat("a", 63))},
		{name: "64 chars rejected", net: svc(strings.Repeat("a", 64)), wantErr: true},
		{name: "dot never resolves", net: svc("a.b"), wantErr: true},
		{name: "space never resolves", net: svc("a b"), wantErr: true},
		{name: "leading dash rejected", net: svc("-a"), wantErr: true},
		{name: "trailing dash rejected", net: svc("a-"), wantErr: true},
		{name: "duplicate rejected", net: svc("b", "b"), wantErr: true},
		{name: "case-folded duplicate rejected", net: svc("b", "B"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseConfig(tt.net, 1)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("parseConfig err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// The agent clears a departed node by pushing an empty (or nil) NetDesired.
// Both shapes must apply cleanly and wipe the previous state.
func TestApplyConfigEmptyClearsState(t *testing.T) {
	for _, tt := range []struct {
		name string
		net  *klitev1.NetDesired
	}{
		{name: "empty message", net: &klitev1.NetDesired{}},
		{name: "nil message", net: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(nil)
			if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
				t.Fatal(err)
			}
			if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: tt.net}); err != nil {
				t.Fatalf("empty push rejected: %v", err)
			}
			cfg := srv.cfg.Load()
			if len(cfg.vips) != 0 || len(cfg.targets) != 0 {
				t.Fatalf("state not cleared: %d vips, %d targets", len(cfg.vips), len(cfg.targets))
			}
			m := ask(srv.dns, udpRemote(), "b.svc.klite.", dns.TypeA, dns.ClassINET)
			if m.Rcode != dns.RcodeNameError {
				t.Fatalf("rcode after clear = %s, want NXDOMAIN", dns.RcodeToString[m.Rcode])
			}
		})
	}
}

func TestApplyConfigDrivesDNS(t *testing.T) {
	srv := testServer(nil)
	if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
		t.Fatal(err)
	}
	m := ask(srv.dns, udpRemote(), "b.svc.klite.", dns.TypeA, dns.ClassINET)
	if len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "10.44.64.9" {
		t.Fatalf("answer = %v", m.Answer)
	}
}

func TestHealthReportsVIPsAndDNS(t *testing.T) {
	binds := make(chan []netip.Addr, 1)
	srv := testServer(func(iface string, want []netip.Addr) (int, error) {
		if iface != "test0" {
			return 0, fmt.Errorf("wrong iface %s", iface)
		}
		binds <- want
		return len(want), nil
	})
	if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
		t.Fatal(err)
	}
	if want := <-binds; len(want) != 1 || want[0] != netip.MustParseAddr("10.44.64.9") {
		t.Fatalf("bindVIPs got %v", want)
	}
	h, err := srv.Health(context.Background(), &klitev1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if h.GetVipsBound() != 1 {
		t.Errorf("vips_bound = %d, want 1", h.GetVipsBound())
	}
	if h.GetDnsReady() {
		t.Error("dns_ready = true with no listeners running")
	}
}

func TestHealthVIPBindFailure(t *testing.T) {
	srv := testServer(func(string, []netip.Addr) (int, error) {
		return 0, fmt.Errorf("netlink says no")
	})
	if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
		t.Fatalf("bind failures must not fail ApplyConfig: %v", err)
	}
	h, _ := srv.Health(context.Background(), &klitev1.HealthRequest{})
	if h.GetVipsBound() != 0 {
		t.Errorf("vips_bound = %d, want 0", h.GetVipsBound())
	}
}

func TestProbesRPC(t *testing.T) {
	d := &fakeDialer{up: map[string]bool{"10.44.128.9:8080": true}}
	srv := New(Options{
		DNSListen: ":0", Upstream: "127.0.0.1:1", Iface: "test0",
		dial: d.dial, bindVIPs: func(_ string, w []netip.Addr) (int, error) { return len(w), nil },
	})
	if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
		t.Fatal(err)
	}
	srv.prober.sweep(context.Background())
	resp, err := srv.Probes(context.Background(), &klitev1.ProbesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetProbes()) != 1 || !resp.GetProbes()[0].GetReady() {
		t.Fatalf("probes = %v", resp.GetProbes())
	}
}

// The DNS handler must keep answering consistently while configs swap under it.
func TestDNSReadsDuringSwaps(t *testing.T) {
	srv := testServer(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := srv.ApplyConfig(ctx, &klitev1.ApplyConfigRequest{Net: desired(1)}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := srv.ApplyConfig(ctx, &klitev1.ApplyConfigRequest{Net: desired(i%60 + 1)}); err != nil {
				t.Errorf("ApplyConfig: %v", err)
				return
			}
		}
	}()
	for range 500 {
		m := ask(srv.dns, udpRemote(), "b.svc.klite.", dns.TypeA, dns.ClassINET)
		if m == nil || m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
			t.Fatalf("bad reply during swap: %v", m)
		}
		a := m.Answer[0].(*dns.A)
		if !vipPool.Contains(netip.MustParseAddr(a.A.String())) {
			t.Fatalf("answer %s outside vip pool", a.A)
		}
	}
	close(stop)
	wg.Wait()
}

// The ring lives on the same server the agent talks to: answers served by
// DNS come back out through the RecentQueries RPC with the cursor applied.
func TestRecentQueriesRPC(t *testing.T) {
	srv := testServer(nil)
	if _, err := srv.ApplyConfig(context.Background(), &klitev1.ApplyConfigRequest{Net: desired(9)}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if m := ask(srv.dns, udpRemote(), "b.svc.klite.", dns.TypeA, dns.ClassINET); m == nil {
			t.Fatal("no reply written")
		}
	}
	resp, err := srv.RecentQueries(context.Background(), &klitev1.RecentQueriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	qs := resp.GetQueries()
	if len(qs) != 3 {
		t.Fatalf("queries = %d, want 3", len(qs))
	}
	for _, q := range qs {
		if q.GetSourceIp() != "10.44.128.5" || q.GetService() != "b" || q.GetUnixMs() <= 0 {
			t.Fatalf("query = %v", q)
		}
	}
	// The cursor is inclusive: asking from the newest timestamp returns at
	// least that entry, and a future cursor returns nothing.
	last := qs[len(qs)-1].GetUnixMs()
	resp, err = srv.RecentQueries(context.Background(), &klitev1.RecentQueriesRequest{SinceUnixMs: last})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetQueries()) == 0 {
		t.Fatal("inclusive cursor dropped the newest entry")
	}
	resp, err = srv.RecentQueries(context.Background(), &klitev1.RecentQueriesRequest{SinceUnixMs: last + int64(time.Minute/time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetQueries(); len(got) != 0 {
		t.Fatalf("future cursor returned %v", got)
	}
}
