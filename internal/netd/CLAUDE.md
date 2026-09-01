# internal/netd

The klite-net daemon. It runs inside the donor container of each node's infra pod (ADR 0008) — never on the host — serving DNS, holding VIPs, probing instances, and taking config over a small admin gRPC the node's agent calls.

Invariants:

- Config is memory-only and arrives as full-state ApplyConfig pushes. Restart empty, wait for the agent to re-push. Never persist.
- DNS answer shapes are load-bearing (research/coredns-dns.md): AAAA on an existing service is NODATA with SOA, never NXDOMAIN, or glibc's parallel lookups discard the A record too. TTL 5, zone `svc.klite`, one upstream only.
- VIP operations touch nothing outside 10.44.64.0/18. The container's primary address is sacred.
- The binary ships in a scratch image for linux/arm64. Netlink code carries linux build tags with a darwin stub so `go build ./...` works on the Mac.
- Nothing here may assume the host can reach it, or that it can reach the host.
