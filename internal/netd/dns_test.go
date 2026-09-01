package netd

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

type fakeWriter struct {
	remote net.Addr
	msg    *dns.Msg
}

func (w *fakeWriter) LocalAddr() net.Addr         { return &net.UDPAddr{IP: net.IPv4zero, Port: 53} }
func (w *fakeWriter) RemoteAddr() net.Addr        { return w.remote }
func (w *fakeWriter) WriteMsg(m *dns.Msg) error   { w.msg = m; return nil }
func (w *fakeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *fakeWriter) Close() error                { return nil }
func (w *fakeWriter) TsigStatus() error           { return nil }
func (w *fakeWriter) TsigTimersOnly(bool)         {}
func (w *fakeWriter) Hijack()                     {}

const testSerial = 7

func testDNSServer(t *testing.T, upstream string) *dnsServer {
	t.Helper()
	cfg, err := parseConfig(&klitev1.NetDesired{Services: []*klitev1.ServiceVIP{
		{Service: "b", Vip: "10.44.64.9", Port: 8080},
	}}, testSerial)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	ptr := &atomic.Pointer[netConfig]{}
	ptr.Store(cfg)
	return newDNSServer(":0", upstream, ptr)
}

func ask(s *dnsServer, remote net.Addr, name string, qtype, qclass uint16) *dns.Msg {
	r := new(dns.Msg)
	r.SetQuestion(name, qtype)
	r.Question[0].Qclass = qclass
	w := &fakeWriter{remote: remote}
	s.serveDNS(w, r)
	return w.msg
}

func udpRemote() net.Addr { return &net.UDPAddr{IP: net.IPv4(10, 44, 128, 5), Port: 40000} }

func TestServeZone(t *testing.T) {
	tests := []struct {
		name       string
		qname      string
		qtype      uint16
		qclass     uint16
		rcode      int
		wantA      string // non-empty: exactly one A answer with this IP
		wantSOANs  bool   // SOA present in authority
		wantSOAAns bool   // SOA present in answer
	}{
		{name: "A hit", qname: "b.svc.klite.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, wantA: "10.44.64.9"},
		{name: "A hit case folded", qname: "B.SVC.Klite.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, wantA: "10.44.64.9"},
		{name: "AAAA existing is NODATA", qname: "b.svc.klite.", qtype: dns.TypeAAAA, rcode: dns.RcodeSuccess, wantSOANs: true},
		{name: "TXT existing is NODATA", qname: "b.svc.klite.", qtype: dns.TypeTXT, rcode: dns.RcodeSuccess, wantSOANs: true},
		{name: "unknown name is NXDOMAIN", qname: "nope.svc.klite.", qtype: dns.TypeA, rcode: dns.RcodeNameError, wantSOANs: true},
		{name: "unknown AAAA is NXDOMAIN", qname: "nope.svc.klite.", qtype: dns.TypeAAAA, rcode: dns.RcodeNameError, wantSOANs: true},
		{name: "multi-label is NXDOMAIN", qname: "x.b.svc.klite.", qtype: dns.TypeA, rcode: dns.RcodeNameError, wantSOANs: true},
		{name: "apex SOA answered", qname: "svc.klite.", qtype: dns.TypeSOA, rcode: dns.RcodeSuccess, wantSOAAns: true},
		{name: "apex A is NODATA", qname: "svc.klite.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, wantSOANs: true},
		{name: "AXFR refused", qname: "svc.klite.", qtype: dns.TypeAXFR, rcode: dns.RcodeRefused},
		{name: "non-IN class refused", qname: "b.svc.klite.", qtype: dns.TypeA, qclass: dns.ClassCHAOS, rcode: dns.RcodeRefused},
	}
	s := testDNSServer(t, "127.0.0.1:1")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.qclass == 0 {
				tt.qclass = dns.ClassINET
			}
			m := ask(s, udpRemote(), tt.qname, tt.qtype, tt.qclass)
			if m == nil {
				t.Fatal("no reply written")
			}
			if m.Rcode != tt.rcode {
				t.Fatalf("rcode = %s, want %s", dns.RcodeToString[m.Rcode], dns.RcodeToString[tt.rcode])
			}
			if tt.rcode != dns.RcodeRefused && !m.Authoritative {
				t.Error("reply not authoritative")
			}
			checkAnswer(t, m, tt.wantA, tt.wantSOAAns)
			checkAuthority(t, m, tt.wantSOANs)
		})
	}
}

func checkAnswer(t *testing.T, m *dns.Msg, wantA string, wantSOA bool) {
	t.Helper()
	switch {
	case wantA != "":
		if len(m.Answer) != 1 {
			t.Fatalf("answers = %d, want 1", len(m.Answer))
		}
		a, ok := m.Answer[0].(*dns.A)
		if !ok {
			t.Fatalf("answer is %T, want *dns.A", m.Answer[0])
		}
		if a.A.String() != wantA {
			t.Errorf("A = %s, want %s", a.A, wantA)
		}
		if a.Hdr.Ttl != recordTTL {
			t.Errorf("ttl = %d, want %d", a.Hdr.Ttl, recordTTL)
		}
		// 0x20-style checks: the answer must echo the query's case.
		if a.Hdr.Name != m.Question[0].Name {
			t.Errorf("answer name %q doesn't echo query %q", a.Hdr.Name, m.Question[0].Name)
		}
	case wantSOA:
		if len(m.Answer) != 1 {
			t.Fatalf("answers = %d, want 1", len(m.Answer))
		}
		checkSOA(t, m.Answer[0])
	default:
		if len(m.Answer) != 0 {
			t.Errorf("answers = %v, want none", m.Answer)
		}
	}
}

func checkAuthority(t *testing.T, m *dns.Msg, wantSOA bool) {
	t.Helper()
	if !wantSOA {
		if len(m.Ns) != 0 {
			t.Errorf("authority = %v, want empty", m.Ns)
		}
		return
	}
	if len(m.Ns) != 1 {
		t.Fatalf("authority has %d records, want 1 SOA", len(m.Ns))
	}
	checkSOA(t, m.Ns[0])
}

func checkSOA(t *testing.T, rr dns.RR) {
	t.Helper()
	soa, ok := rr.(*dns.SOA)
	if !ok {
		t.Fatalf("record is %T, want *dns.SOA", rr)
	}
	if soa.Ns != "ns.svc.klite." || soa.Mbox != "hostmaster.svc.klite." {
		t.Errorf("SOA ns/mbox = %s/%s", soa.Ns, soa.Mbox)
	}
	if soa.Serial != testSerial {
		t.Errorf("SOA serial = %d, want %d", soa.Serial, testSerial)
	}
	if soa.Minttl != recordTTL || soa.Hdr.Ttl != recordTTL {
		t.Errorf("SOA ttl/minttl = %d/%d, want %d", soa.Hdr.Ttl, soa.Minttl, recordTTL)
	}
	if soa.Refresh != 7200 || soa.Retry != 1800 || soa.Expire != 86400 {
		t.Errorf("SOA timers = %d %d %d", soa.Refresh, soa.Retry, soa.Expire)
	}
}

// A panicking zone handler must yield SERVFAIL, not a dead resolver. The
// panic is injected via an extra mux route so the recovery wrapper is
// exercised exactly as a real handler bug would hit it.
func TestServeDNSRecoversPanic(t *testing.T) {
	s := testDNSServer(t, "127.0.0.1:1")
	s.mux.HandleFunc("boom.test.", func(dns.ResponseWriter, *dns.Msg) {
		panic("handler bug")
	})
	m := ask(s, udpRemote(), "boom.test.", dns.TypeA, dns.ClassINET)
	if m == nil {
		t.Fatal("no reply written after handler panic")
	}
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
}

// NOTIFY passes miekg's default accept func, so serveDNS has to turn it away
// itself rather than answering it like a query.
func TestServeDNSRejectsNonQueryOpcode(t *testing.T) {
	s := testDNSServer(t, "127.0.0.1:1")
	r := new(dns.Msg)
	r.SetQuestion("b.svc.klite.", dns.TypeA)
	r.Opcode = dns.OpcodeNotify
	w := &fakeWriter{remote: udpRemote()}
	s.serveDNS(w, r)
	if w.msg == nil {
		t.Fatal("no reply written")
	}
	if w.msg.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("rcode = %s, want NOTIMP", dns.RcodeToString[w.msg.Rcode])
	}
	if len(w.msg.Answer) != 0 {
		t.Fatalf("NOTIFY got answers: %v", w.msg.Answer)
	}
}

// run must flip ready once both listeners are up and return nil when asked
// to stop. A stop-path shutdown error is noise, not a failure.
func TestDNSRunLifecycle(t *testing.T) {
	s := testDNSServer(t, "127.0.0.1:1")
	s.listen = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !s.ready() {
		if time.Now().After(deadline) {
			t.Fatal("listeners never came up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

// A listener that can't bind must surface as an error, not hang the daemon.
func TestDNSRunListenFailure(t *testing.T) {
	s := testDNSServer(t, "127.0.0.1:1")
	s.listen = "256.256.256.256:0"
	if err := s.run(context.Background()); err == nil {
		t.Fatal("run with an unbindable address returned nil")
	}
}

func TestClientBufSize(t *testing.T) {
	tests := []struct {
		name string
		opt  uint16 // 0: no EDNS0
		want int
	}{
		{name: "no edns0 means 512", want: dns.MinMsgSize},
		{name: "edns0 above 512 honored", opt: 1232, want: 1232},
		{name: "edns0 at 512 stays 512", opt: 512, want: dns.MinMsgSize},
		{name: "edns0 below 512 clamped up", opt: 200, want: dns.MinMsgSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := new(dns.Msg)
			r.SetQuestion("example.com.", dns.TypeA)
			if tt.opt != 0 {
				r.SetEdns0(tt.opt, false)
			}
			if got := clientBufSize(r); got != tt.want {
				t.Errorf("clientBufSize = %d, want %d", got, tt.want)
			}
		})
	}
}

// fakeUpstream runs a real miekg server answering a fixed A record, on UDP
// and TCP, and returns its address.
func fakeUpstream(t *testing.T) string {
	t.Helper()
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.IPv4(93, 184, 216, 34),
		}}
		_ = w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	udp := &dns.Server{PacketConn: pc, Handler: handler}
	tcp := &dns.Server{Listener: ln, Handler: handler}
	go udp.ActivateAndServe() //nolint:errcheck
	go tcp.ActivateAndServe() //nolint:errcheck
	t.Cleanup(func() {
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
	})
	return pc.LocalAddr().String()
}

func TestForward(t *testing.T) {
	s := testDNSServer(t, fakeUpstream(t))
	for _, tt := range []struct {
		name   string
		remote net.Addr
	}{
		{name: "udp", remote: udpRemote()},
		{name: "tcp", remote: &net.TCPAddr{IP: net.IPv4(10, 44, 128, 5), Port: 40000}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := ask(s, tt.remote, "example.com.", dns.TypeA, dns.ClassINET)
			if m == nil {
				t.Fatal("no reply written")
			}
			if m.Rcode != dns.RcodeSuccess {
				t.Fatalf("rcode = %s", dns.RcodeToString[m.Rcode])
			}
			a, ok := m.Answer[0].(*dns.A)
			if !ok || a.A.String() != "93.184.216.34" {
				t.Fatalf("answer = %v", m.Answer)
			}
		})
	}
}

func TestForwardUpstreamDown(t *testing.T) {
	s := testDNSServer(t, "127.0.0.1:1")
	s.udpc.Timeout = 250 * time.Millisecond
	m := ask(s, udpRemote(), "example.com.", dns.TypeA, dns.ClassINET)
	if m == nil {
		t.Fatal("no reply written: a dead upstream must still produce SERVFAIL")
	}
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
}
