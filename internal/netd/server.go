package netd

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Options configures the daemon. Zero values take the documented defaults.
type Options struct {
	DNSListen string // udp+tcp DNS listen address (default :53)
	Upstream  string // upstream resolver for non-klite names (default 1.1.1.1:53)
	Iface     string // interface holding the VIPs (default eth0)

	dial     dialFunc                                // test hook
	bindVIPs func(string, []netip.Addr) (int, error) // test hook
}

// Server implements KliteNetService and owns the DNS server, VIP
// reconciliation, and the prober. The agent is its only client.
type Server struct {
	klitev1.UnimplementedKliteNetServiceServer

	iface     string
	applyMu   sync.Mutex // serializes ApplyConfig pushes
	cfg       atomic.Pointer[netConfig]
	serial    atomic.Uint32
	vipsBound atomic.Int32
	dns       *dnsServer
	prober    *prober
	queries   *queryLog
	bindVIPs  func(string, []netip.Addr) (int, error)
}

// New builds a Server. Nothing persists across restarts by design: the state
// is empty until the agent's first ApplyConfig push.
func New(opts Options) *Server {
	if opts.DNSListen == "" {
		opts.DNSListen = ":53"
	}
	if opts.Upstream == "" {
		opts.Upstream = "1.1.1.1:53"
	}
	if opts.Iface == "" {
		opts.Iface = "eth0"
	}
	if opts.bindVIPs == nil {
		opts.bindVIPs = bindVIPs
	}
	s := &Server{iface: opts.Iface, bindVIPs: opts.bindVIPs}
	s.cfg.Store(emptyConfig())
	s.queries = newQueryLog()
	s.dns = newDNSServer(opts.DNSListen, opts.Upstream, &s.cfg, s.queries)
	s.prober = newProber(&s.cfg, opts.dial)
	return s
}

// Run serves DNS and probes until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.dns.run(gctx) })
	g.Go(func() error { return s.prober.run(gctx) })
	return g.Wait()
}

// ApplyConfig swaps in the pushed full state and reconciles VIPs and probes
// against it. A VIP bind failure is not an RPC error, it shows up as
// vips_bound in Health.
//
// Pushes are serialized: the agent retries a timed-out ApplyConfig while the
// first may still be running, and without the lock the older config could be
// stored last and stick.
func (s *Server) ApplyConfig(_ context.Context, req *klitev1.ApplyConfigRequest) (*klitev1.ApplyConfigResponse, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	cfg, err := parseConfig(req.GetNet(), s.serial.Add(1))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "net config: %v", err)
	}
	s.cfg.Store(cfg)
	s.prober.kickNow()
	s.reconcileVIPs()
	slog.Info("config applied", "serial", cfg.serial, "services", len(cfg.vips),
		"probe_targets", len(cfg.targets), "vips_bound", s.vipsBound.Load())
	return &klitev1.ApplyConfigResponse{}, nil
}

func (s *Server) reconcileVIPs() {
	cfg := s.cfg.Load()
	bound, err := s.bindVIPs(s.iface, cfg.vipList)
	if err != nil {
		slog.Error("vip reconcile", "iface", s.iface, "err", err)
	}
	s.vipsBound.Store(int32(bound))
}

// Probes returns the latest probe result per instance.
func (s *Server) Probes(context.Context, *klitev1.ProbesRequest) (*klitev1.ProbesResponse, error) {
	return &klitev1.ProbesResponse{Probes: s.prober.snapshot()}, nil
}

// Health reports whether both DNS listeners are up and how many VIPs are bound.
func (s *Server) Health(context.Context, *klitev1.HealthRequest) (*klitev1.HealthResponse, error) {
	return &klitev1.HealthResponse{
		DnsReady:  s.dns.ready(),
		VipsBound: s.vipsBound.Load(),
	}, nil
}

// RecentQueries returns the in-zone A answers kdns served since the caller's
// cursor (inclusive), oldest first, capped by the ring's ~30s retention.
// Like every RPC here it answers on the admin listener, so the facade
// reaches it on the donor's published localhost port.
func (s *Server) RecentQueries(_ context.Context, req *klitev1.RecentQueriesRequest) (*klitev1.RecentQueriesResponse, error) {
	return &klitev1.RecentQueriesResponse{Queries: s.queries.since(req.GetSinceUnixMs())}, nil
}
