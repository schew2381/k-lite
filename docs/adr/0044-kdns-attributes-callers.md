# kdns attributes callers for the traffic feed

ADR 0041 shipped the live board with a hole it named: Envoy's counters can't say which instance placed a call, so live events carry an empty `fromInstance` and the trace starts at kdns. The live board wants the caller back. One observation pays for it: every chatty call opens with a fresh A lookup, since busybox wget resolves per invocation and ADR 0017's TTL is 5 seconds. kdns already sees that query's source IP, which is the calling instance's address on the bridge. So klite-net now keeps a small in-memory ring of the in-zone A answers it serves (source IP, service label, unix milliseconds), and an additive `RecentQueries` RPC on the existing admin listener hands it out. The ring holds 4096 entries, roughly 30 seconds of history. The facade polls each donor's published `1900X` port from the host (the ADR 0029 lockdown leaves that path open on purpose) and joins source IPs to instances through its snapshot.

## Considered Options

1. **Envoy TCP access logs.** They have the highest fidelity, since every log line names the downstream address, but nothing collects access logs today. Turning them on means editing the agent's Envoy bootstrap, then tailing and parsing a log stream out of every infra container. ADR 0041 already declined that plumbing for the feed itself, and one extra field doesn't change the price.
2. **Per-listener Envoy stats.** Already polled every second, but structurally unable to attribute: one Envoy fronts every instance on its node, and a counter keeps no memory of who dialed it. This is the exact hole ADR 0041 documented.
3. **A recent-queries ring in kdns** (chosen). The resolver is the one component that sees caller and target in the same packet. One ring, one additive RPC, the admin listener that already exists.

## Consequences

- Attribution keys on DNS lookups, not connections. A client that caches DNS past the 5-second TTL under-attributes (its calls leave no fresh entry), while one that resolves and never dials over-attributes. The seeded apps do neither, so the demo board reads true. For arbitrary workloads the ring is best-effort.
- The ring is observability, never authorization. Allowed and denied pairs resolve identically (denial happens later, at Envoy's RBAC filters), so recording is policy-blind and nothing in enforcement may ever read it.
- Recording sits on the DNS hot path, so it's one short mutex hold per served A answer. Entries age out on read after roughly 30 seconds and the 4096-entry cap bounds memory. Like all of klite-net's state the ring is memory-only. A donor restart forgets, which a 30-second feed can afford.
- Running donors keep their old binary. The donor's config hash covers the image tag, not its content, so `make net-image` alone recreates nothing. The local rollout is `hack/dev-down.sh && hack/dev-up.sh`, since dev-up rebuilds `klite-net:dev` and fresh donors adopt it. Under LAN mode the server pins the ghcr image for every donor (local ones included), so that path, like a remote machine's, waits on the next tagged release (ADR 0038).
