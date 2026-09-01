# Ingress ports are allocations, and listeners follow allocations, not health

Cross-machine ingress (0034) needed a port per local endpoint from each node's published slice. Ports live as a server-materialized `IngressAllocation` kind (`<service>.<instance>`), reconciled by a leader-only allocator copying the VIP allocator's reserve-and-repair shape (ADR 0022), fixed at instance birth so Ready↔Draining transitions never move them. The destination Envoy's ingress listeners are generated from the allocation list, not from endpoint health. A listener exists before traffic routes to it and survives while its instance drains, which is what keeps drains hitless across the mTLS hop.

## Considered Options

1. **Ports in InstanceStatus.** Zero new kinds, but it fate-shares a server-owned allocation with agent-written status, the exact shape ADR 0022 rejected for VIPs.
2. **Listeners derived from endpoint health.** Simpler wiring, and it reintroduces cross-stream LDS/EDS skew during churn: the listener vanishes while connections still drain.
3. **A dedicated kind driving an allocation-based listener list** (chosen).

## Consequences

- `klite get ingressallocations` shows the port map, and Apply rejects the kind like the other server-materialized ones.
- Remote endpoints render as `machineAddress:ingressPort` against a constant `transport_socket_matches` pair. Regenerating the match list from live endpoints would churn CDS and drain connections mid-rollout, so it never varies.
- A remote endpoint without its allocation is omitted from EDS entirely. The flat-bridge path is dead, not a fallback.
- The kind registration surfaced a codec gap (a forged YAML could nil-panic klited through the envelope switch), now closed and pinned by tests.
