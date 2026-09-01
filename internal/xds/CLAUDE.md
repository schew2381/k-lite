# internal/xds

Translates a node's NetDesired into Envoy ADS snapshots and owns the snapshot cache klited serves. This is the miniature-istiod half of ADR 0007 — the working reference is hack/spike-envoy plus research/envoy-xds.md.

Invariants:

- The snapshot's node key must equal the Envoy bootstrap `node.id` exactly, and the version string must change on every content change. Envoy ignores re-pushes of an ACKed version.
- `snapshot.Consistent()` gates every set. CDS/EDS/LDS names cross-reference or nothing applies.
- Three settings are deliberate and must survive refactors: `freebind` on VIP listeners (bind-before-VIP-exists; ingress listeners bind 0.0.0.0 and deliberately skip it), `healthy_panic_threshold: 0` on clusters (the 50% default breaks draining at replicas=2), and `idle_timeout: 0` on tcp_proxy (the 1h default leaks connections).
- Output is deterministic (sorted principals, stable policy keys). RBAC policies with zero principals get dropped, but the ALLOW filter survives allowlist-mode even when empty — that emptiness is what enforces the flip.
- Cross-node (M9, ADR 0034): remote endpoints render as machineAddress:ingressPort tagged for the cluster's transport_socket_matches. A remote endpoint without its rider is omitted, never emitted as a raw pod IP. The match list is constant on purpose — regenerating it from live endpoints would churn CDS and drain connections mid-rollout. Ingress listeners come from NetDesired's allocation-driven ingress_listeners list, not from endpoint groups, so they predate routing and survive draining. Certificate paths under /etc/klite/tls mirror the agent's mount and load at resource creation: rotating files without recreating the Envoy container changes nothing.
