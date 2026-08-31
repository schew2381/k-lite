package netd

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
)

const (
	zone          = "svc.klite."
	recordTTL     = 5
	forwardBudget = 2 * time.Second
)

// dnsServer answers svc.klite authoritatively from the current netConfig and
// forwards everything else to one upstream (research/coredns-dns.md).
type dnsServer struct {
	listen   string
	upstream string
	cfg      *atomic.Pointer[netConfig]
	udpc     *dns.Client
	tcpc     *dns.Client
	mux      *dns.ServeMux
	started  atomic.Int32 // listeners up, out of 2 (udp+tcp)
}

func newDNSServer(listen, upstream string, cfg *atomic.Pointer[netConfig]) *dnsServer {
	s := &dnsServer{
		listen:   listen,
		upstream: upstream,
		cfg:      cfg,
		udpc:     &dns.Client{Timeout: forwardBudget},
		tcpc:     &dns.Client{Net: "tcp", Timeout: forwardBudget},
		mux:      dns.NewServeMux(),
	}
	s.mux.HandleFunc(zone, s.serveZone)
	s.mux.HandleFunc(".", s.forward)
	return s
}

func (s *dnsServer) ready() bool { return s.started.Load() == 2 }

func (s *dnsServer) run(ctx context.Context) error {
	h := dns.HandlerFunc(s.serveDNS)
	onStart := func() { s.started.Add(1) }
	udp := &dns.Server{Addr: s.listen, Net: "udp", Handler: h, NotifyStartedFunc: onStart}
	tcp := &dns.Server{Addr: s.listen, Net: "tcp", Handler: h, NotifyStartedFunc: onStart}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(udp.ListenAndServe)
	g.Go(tcp.ListenAndServe)
	g.Go(func() error {
		<-gctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return errors.Join(udp.ShutdownContext(sctx), tcp.ShutdownContext(sctx))
	})
	err := g.Wait()
	if ctx.Err() != nil {
		err = nil // asked to stop: shutdown-path errors are noise
	}
	return err
}

// serveDNS wraps the mux with per-query panic recovery so one malformed
// query can't take down the node's resolver.
func (s *dnsServer) serveDNS(w dns.ResponseWriter, r *dns.Msg) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("dns handler panic", "panic", p)
			reply(w, new(dns.Msg).SetRcode(r, dns.RcodeServerFailure))
		}
	}()
	s.mux.ServeDNS(w, r)
}

func (s *dnsServer) serveZone(w dns.ResponseWriter, r *dns.Msg) {
	q := r.Question[0]
	if q.Qclass != dns.ClassINET || q.Qtype == dns.TypeAXFR || q.Qtype == dns.TypeIXFR {
		reply(w, new(dns.Msg).SetRcode(r, dns.RcodeRefused))
		return
	}
	cfg := s.cfg.Load()
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	// Lowercase only the lookup key, since SetReply already echoed the query's case.
	name := dns.CanonicalName(q.Name)
	switch {
	case name == zone && q.Qtype == dns.TypeSOA:
		m.Answer = []dns.RR{soaRR(q.Name, cfg.serial)}
	case name == zone:
		nodata(m, cfg.serial)
	default:
		s.serveService(m, q, name, cfg)
	}
	reply(w, m)
}

func (s *dnsServer) serveService(m *dns.Msg, q dns.Question, name string, cfg *netConfig) {
	vip, ok := lookup(cfg, name)
	switch {
	case !ok:
		// An unknown in-zone name gets a fast authoritative NXDOMAIN,
		// because the stub's search-domain fallback waits on it.
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{soaRR(zone, cfg.serial)}
	case q.Qtype == dns.TypeA:
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: recordTTL},
			A:   vip.AsSlice(),
		}}
	default:
		// AAAA (and any other type) for an existing service must be NODATA,
		// never NXDOMAIN, or glibc's parallel A lookup dies with it.
		nodata(m, cfg.serial)
	}
}

// lookup resolves a canonical (lowercase, fqdn) in-zone name to a VIP.
func lookup(cfg *netConfig, name string) (netip.Addr, bool) {
	label, found := strings.CutSuffix(name, "."+zone)
	if !found || label == "" || strings.Contains(label, ".") {
		return netip.Addr{}, false
	}
	vip, ok := cfg.vips[label]
	return vip, ok
}

func nodata(m *dns.Msg, serial uint32) {
	m.Ns = []dns.RR{soaRR(zone, serial)}
}

func soaRR(name string, serial uint32) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: recordTTL},
		Ns:      "ns." + zone,
		Mbox:    "hostmaster." + zone,
		Serial:  serial,
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  recordTTL,
	}
}

// forward proxies non-klite names to the upstream on the transport the query
// arrived on. Truncation passes through: the client retries us over TCP and
// that leg forwards over TCP.
func (s *dnsServer) forward(w dns.ResponseWriter, r *dns.Msg) {
	c, tcp := s.udpc, false
	if w.RemoteAddr() != nil && w.RemoteAddr().Network() == "tcp" {
		c, tcp = s.tcpc, true
	}
	in, _, err := c.Exchange(r, s.upstream)
	if err != nil {
		slog.Warn("upstream exchange failed", "upstream", s.upstream, "err", err)
		reply(w, new(dns.Msg).SetRcode(r, dns.RcodeServerFailure))
		return
	}
	if !tcp {
		in.Truncate(clientBufSize(r))
	}
	reply(w, in)
}

// clientBufSize is the largest UDP reply the client advertised it can take.
func clientBufSize(r *dns.Msg) int {
	if opt := r.IsEdns0(); opt != nil && int(opt.UDPSize()) > dns.MinMsgSize {
		return int(opt.UDPSize())
	}
	return dns.MinMsgSize
}

// reply always writes something: a dropped query leaves the stub waiting out
// its full timeout.
func reply(w dns.ResponseWriter, m *dns.Msg) {
	if err := w.WriteMsg(m); err != nil {
		slog.Warn("dns write failed", "err", err)
	}
}
