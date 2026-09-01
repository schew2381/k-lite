# VIP allocations are a first-class stored kind

Each (Service, Node) VIP lives as a server-materialized `VIPAllocation` object named `<service>.<node>`, reconciled by a leader-only controller that allocates from 10.44.64.0/18 and releases when the service or node goes away. M4's integration surfaced the need: the endpoints engine on every replica must read identical VIPs, so the assignment has to be durable, CAS-safe state rather than anything computed or cached.

## Considered Options

1. **Hash-derived VIPs** (service+node hashed into the pool). No storage at all, but collisions need a probing scheme anyway, and then the "stateless" mapping isn't derivable without replaying it.
2. **A map inside Service status.** It fate-shares user-applied spec with server-owned allocations, and every node join churns a CAS on every Service.
3. **A dedicated kind** (chosen). Same store, watch, and GC machinery as everything else, and `klite get vipallocations` makes the allocator debuggable for free.

## Consequences

- Apply rejects the kind — it's server-materialized, like Instance.
- The allocator skips network and broadcast addresses and only ever hands out pool members.
- Deleting a Service or Node releases its allocations through the normal controller path, so leaked VIPs are a visible bug, not silent exhaustion.
