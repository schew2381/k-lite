# kdns implementation notes (ADRs 0006, 0017)

kdns answers `<service>.svc.klite` with the asking node's per-service VIP at TTL 5 and forwards everything else to 1.1.1.1. Names resolve even for callers a policy denies, because the refusal happens at the connection. These notes pin that behavior to specific code in miekg/dns and CoreDNS, both cloned under `~/code/`.

## Server skeleton

```go
mux := dns.NewServeMux()
mux.HandleFunc("svc.klite.", s.serveZone) // longest-suffix match, case-folded
mux.HandleFunc(".", s.forward)            // root fallback for everything else

udp := &dns.Server{Addr: ":53", Net: "udp", Handler: mux}
tcp := &dns.Server{Addr: ":53", Net: "tcp", Handler: mux}
g, gctx := errgroup.WithContext(ctx)
g.Go(udp.ListenAndServe)
g.Go(tcp.ListenAndServe)
g.Go(func() error {
	<-gctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return errors.Join(udp.ShutdownContext(sctx), tcp.ShutdownContext(sctx))
})
```

We need two `dns.Server` values on one mux because `Net` picks a single transport (miekg-dns/server.go:206). The mux folds each query name through `CanonicalName` and routes by longest registered suffix, so `B.SVC.KLITE.` still reaches the zone handler and anything unmatched falls to `"."` (serve_mux.go:38-53). The server spawns a goroutine per UDP packet and per TCP connection (server.go:554, 492), so handlers run concurrently and can't touch mutable shared state. The zone handler reads its records through an atomic pointer instead:

```go
type zone struct{ vips map[string]netip.Addr } // immutable once stored

var current atomic.Pointer[zone]
// handler: z := current.Load(); vip, ok := z.vips[key]
// updater: builds a fresh map, then current.Store(&zone{vips: m})
```

Rebuilding the map on every service event costs nothing at our scale and keeps locks off the hot path.

## Response shapes

Every reply starts with `m := new(dns.Msg); m.SetReply(r)`, which copies the query ID and echoes the question with the client's original case intact (miekg-dns/defaults.go:15-28). Lowercase only the map key, never the reply.

| Query for `b.svc.klite.`   | Rcode        | Answer             | Authority |
| -------------------------- | ------------ | ------------------ | --------- |
| A, service exists          | NOERROR (AA) | one `dns.A`, TTL 5 | empty     |
| AAAA, service exists       | NOERROR (AA) | empty              | SOA       |
| TXT/MX/SRV, service exists | NOERROR (AA) | empty              | SOA       |
| any type, unknown name     | NXDOMAIN (AA)| empty              | SOA       |
| SOA at `svc.klite.`        | NOERROR (AA) | the SOA            | empty     |
| AXFR, or class not IN      | REFUSED      | empty              | empty     |

```go
m.Authoritative = true
m.Answer = []dns.RR{&dns.A{
	Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA,
		Class: dns.ClassINET, Ttl: 5},
	A: vip.AsSlice(),
}}
```

AAAA for our IPv4-only services must be NODATA, an empty NOERROR with the SOA in the authority section, never NXDOMAIN. NXDOMAIN asserts the *name* has no records of any type. glibc's `getaddrinfo` fires A and AAAA in parallel, so an NXDOMAIN on the AAAA leg tells the stub the whole name is gone and the A answer dies with it. CoreDNS enforces this by running a fake A lookup for every type it doesn't serve, letting name existence pick between NODATA and NXDOMAIN (coredns/plugin/kubernetes/handler.go:59-62, 79-81).

Both negative shapes carry the SOA so resolvers know how long to cache the miss. RFC 2308 sets that to the smaller of the SOA's TTL and its Minttl field, so we put 5 in both, copying CoreDNS's synthesis (backend_lookup.go:470-489) with our numbers:

```
svc.klite. 5 IN SOA ns.svc.klite. hostmaster.svc.klite. <serial> 7200 1800 86400 5
```

CoreDNS's kubernetes plugin defaults its record TTL to 5 as well (kubernetes.go:90), the same value ADR 0017 picked.

## Forwarding

```go
var udpc = &dns.Client{Timeout: 3 * time.Second}
var tcpc = &dns.Client{Net: "tcp", Timeout: 3 * time.Second}

func (s *server) forward(w dns.ResponseWriter, r *dns.Msg) {
	c := udpc
	if w.RemoteAddr().Network() == "tcp" {
		c = tcpc
	}
	in, _, err := c.Exchange(r, "1.1.1.1:53")
	if err != nil {
		m := new(dns.Msg)
		w.WriteMsg(m.SetRcode(r, dns.RcodeServerFailure))
		return
	}
	w.WriteMsg(in)
}
```

`Client.Exchange` never retries and never falls back to TCP on truncation, by documented design (miekg-dns/client.go:161-163). So we forward on the transport the query arrived on and pass the TC bit through untouched. A truncated UDP answer then sends the client back to us over TCP, and that leg forwards over TCP. That's CoreDNS's default, and its `prefer_udp` mode shows the alternative of retrying upstream over TCP yourself (forward/forward.go:197-200). Always write *something* on error, since a dropped query leaves the stub waiting out its full timeout.

## What CoreDNS teaches

Embedding CoreDNS means adopting its Caddy-fork lifecycle (`coremain` wraps Corefile parsing and server plumbing), a code-generated chain of 61 compiled-in plugin directives (plugin.cfg), a 222-line go.mod pulling prometheus, opentelemetry and quic-go, and a seven-method `ServiceBackend` interface just to serve one map (plugin/backend.go:13-37). miekg/dns needs three `golang.org/x` modules, and the handlers above are nearly the whole program. Three behaviors CoreDNS gets right that we copy anyway:

- Fold case for lookups, echo it back in replies. CoreDNS lowercases once for routing (core/dnsserver/server.go:329) and keeps the query's case by slicing the original qname (kubernetes/handler.go:21). Resolvers doing 0x20 randomization send mixed-case queries on purpose and check the echo.
- Answer every type inside the zone authoritatively. Unknown names get NXDOMAIN plus SOA, known names with the wrong type get NODATA plus SOA, and AXFR or a class other than IN gets REFUSED (kubernetes/handler.go:31-63, dnsserver/server.go:316).
- Fit replies to the client's buffer. A ScrubWriter wraps every reply, truncating to the advertised EDNS0 size and compressing only when a UDP reply risks fragmentation, past 1480 bytes on v4 or 1220 on v6 (request/request.go:220-244). miekg/dns's `WriteMsg` does no scrubbing of its own (server.go:747-769), so forwarded replies need `Truncate` before the write even though our one-record answers never will.

CoreDNS also recovers panics per query and answers SERVFAIL (dnsserver/server.go:301-314). We steal that too, because one malformed query must not take down the node's resolver.

## Client cache reality

Alpine images run musl, whose stub resolver caches nothing, applies search domains, and queries every `nameserver` line in parallel, taking the first answer. kdns must be the only nameserver in a container's resolv.conf or a faster upstream can win the race with an NXDOMAIN for our zone. musl before 1.2.4 (Alpine 3.19) also never retries truncated answers over TCP. glibc caches nothing either without nscd or systemd-resolved, and containers run neither.

Caching lives above libc. The JVM holds positive answers for 30 seconds by default (`networkaddress.cache.ttl`) and ignores our TTL entirely, while Go and Node resolvers cache nothing. TTL 5 therefore bounds staleness only for resolvers that honor TTLs at all, and the fixed per-node VIP from ADR 0006 keeps the ignorers correct because the address they hoard never moves.

With `--dns-search svc.klite` and the default `ndots:1`, a program asking for `b` has zero dots, so the stub applies the search list before sending and the wire carries `b.svc.klite.` fully qualified. Wire-format names are always absolute, and kdns never sees a bare `b`. The reverse path matters more. A lookup of an unknown single-label name like `metadata` also arrives as `metadata.svc.klite.`, and the stub tries `metadata.` only after our NXDOMAIN. Fast authoritative NXDOMAIN inside the zone is load-bearing.

## kdns implementation checklist

1. Serve `svc.klite.` and `.` from one shared mux over UDP and TCP on :53 inside klite-net.
2. Swap config through `atomic.Pointer[zone]`, building a fresh immutable map per update, with no locks in handlers.
3. A queries return the asking node's VIP at TTL 5 with AA set, and resolution ignores policy (ADR 0017).
4. AAAA and other types for existing services return NODATA with the SOA above, unknown names return NXDOMAIN with the same SOA.
5. Lowercase lookups, echo query case, refuse AXFR and non-IN classes.
6. Forward everything else to 1.1.1.1 on the inbound transport with a 3s budget, pass TC through, answer SERVFAIL on upstream failure.
7. Scrub forwarded replies to the client's buffer before writing.
8. Recover panics per query and answer SERVFAIL.
9. Render resolv.conf inputs so kdns is the sole nameserver, with `search svc.klite` and `ndots:1`.
