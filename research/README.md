# Research

Twelve documents live here, one per tool or question, and each ends in a verdict for the decision it feeds. Parallel subagents wrote them from shallow clones under `~/code` and primary docs, and every claim that could be run was run on this machine's colima. All prose passed the adversarial `/writing` review CLAUDE.md requires. Two spike scripts under `hack/` are the empirical gates the biggest decisions hung on, and both passed.

## The data-plane matrix

The discovery layer has four jobs:

1. Resolve names.
2. Pick ready endpoints under churn.
3. Give policy a chokepoint that knows the caller.
4. Survive the move to real machines.

The matrix scores each candidate against those four jobs.

| Candidate | Runs without k8s | Dynamic L4 listeners | Endpoint churn without reload | Health and drain states | Policy chokepoint with caller identity | Weight |
|---|---|---|---|---|---|---|
| Envoy + our xDS (chosen, ADR 0007) | yes | yes (LDS + freebind) | yes (EDS) | yes, DRAINING included | yes (RBAC network filter) | ~220MB image plus ~300 lines of our control-plane Go |
| Custom Go proxy (spike-gated fallback) | yes | yes | yes | ours to build | ours to build | smallest, and we'd own every connection-handling bug |
| Traefik | yes | no, entrypoints are static bootstrap config | yes | partial | HTTP-centric | medium |
| Caddy + caddy-l4 | yes | yes (POST /load swaps everything) | yes, as whole-document replacement | resets on every swap, no external drain signal | source-IP matchers in a pre-1.0 module | medium |
| nginx OSS | yes | no, reload model | no | no | no | medium |
| Consul + Connect | yes | through its own managed Envoy | yes | yes | intentions with mTLS identity | 357k lines, raft plus gossip plus an agent per node |
| Istio / Linkerd | no | n/a | n/a | n/a | n/a | n/a |
| Docker network aliases | yes | n/a | yes | none, dead containers linger in answers | none | zero |
| Env-var injection | yes | n/a | no, stale until restart | none | none | zero |
| DNS round-robin to instance IPs | yes | n/a | TTL-bound | none | none | tiny |

The Istio and Linkerd row collapses to one cell deliberately. Workloads can leave Kubernetes but the mesh can't (research/istio-linkerd.md carries the citations), so the rest of the row never gets evaluated.

## Reading order, by decision

| Decision | Start with |
|---|---|
| ADR 0004, gRPC transport | [grpc-go.md](grpc-go.md) |
| ADR 0005, etcd control plane | [etcd.md](etcd.md), [prior-art.md](prior-art.md) |
| ADR 0006 and 0017, DNS plus per-node VIPs | [docker-networking-macos.md](docker-networking-macos.md), [coredns-dns.md](coredns-dns.md) |
| ADR 0007, Envoy from day one | [envoy-xds.md](envoy-xds.md), [proxies-considered.md](proxies-considered.md), [consul.md](consul.md), [istio-linkerd.md](istio-linkerd.md) |
| ADR 0009, policy semantics | [istio-linkerd.md](istio-linkerd.md), [consul.md](consul.md) |
| ADR 0013, join auth | [join-auth.md](join-auth.md) |
| ADR 0016, cross-machine data plane deferred | [overlay-wan.md](overlay-wan.md) |
| M2 runtime implementation | [docker-go-sdk.md](docker-go-sdk.md) |
