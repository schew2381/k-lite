# Envoy is the data plane from day one, speaking real xDS

Each node runs an upstream Envoy programmed by `klited` over xDS via go-control-plane. LDS delivers listeners bound to that node's VIPs, EDS carries endpoint health, and RBAC filters carry policy. We're deliberately building a miniature istiod rather than writing our own proxy and swapping later. The decision is gated: M0's spike must prove Envoy with dynamic VIP listeners and RBAC on colima/arm64, and failure flips this ADR to the fallback below.

## Considered Options

1. **Custom Go L4 proxy first, Envoy as a documented swap.** A few hundred lines we'd fully own, which also means a few hundred lines of connection-handling bugs. The grill declined to pay for those once graceful drain became a requirement. We kept it as the spike-gated fallback.
2. **Envoy + our xDS control plane** (chosen). Draining, per-request gRPC balancing, and hitless config updates arrive as machinery Istio already battle-tested. The code left for us to write is exactly the control plane, which is the part this project is for.
3. **Traefik or Caddy per node.** Their routing config is dynamic, but listener addresses are static or awkward exactly where we need one listener per VIP:port appearing at runtime, and both still need our DNS beside them.
4. **nginx.** A reload-based config model makes it the wrong tool for per-second endpoint churn.
5. **Consul with intentions.** It solves discovery and policy at the price of a second raft-and-gossip system larger than k-lite itself.
6. **Istio or Linkerd wholesale.** Both effectively require Kubernetes underneath, and `research/istio-linkerd.md` carries the citations.

## Consequences

- klited grows an xDS server with a per-node snapshot cache, and M4 depends on it.
- Debugging moves to `envoy config_dump` and admin stats. We trade readable homegrown code for industrial behavior, eyes open.
- The infra pod (ADR 0008) exists partly so Envoy can bind VIPs before they exist on the interface. The listener `freebind` option makes bind order irrelevant.
- If the spike fails, we fall back to option 1, mark this ADR superseded, and re-cut the milestones.

## Outcome

The gate passed on 2026-08-31 (`hack/spike-envoy/`). Envoy v1.31.5 on colima/arm64 took CDS/EDS/LDS over one ADS stream from a host-side go-control-plane server, bound a VIP listener through `freebind` before the address existed, and applied a targeted RBAC deny hitlessly (43 allowed, 2 denied, zero misses under load). The fallback stays recorded but unused. M4 inherits three constraints from the run:

1. Netns-scoped config belongs on the donor container.
2. The xDS bootstrap cluster needs explicit HTTP/2 typed options, with the node id matching the snapshot key exactly.
3. Health checks must gate on `lds.update_success` rather than `connected_state`.
