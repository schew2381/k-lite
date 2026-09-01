# Cross-machine traffic rides node-published ports behind proxy mTLS

This ends ADR 0016's deferral. When an endpoint lives on another node, klited renders it in the consuming node's EDS as `machineAddress:ingressPort` instead of a pod IP. The ingress port belongs to the destination node's Envoy: one mTLS-required listener per local endpoint, bound inside a host-port range the infra-pod donor publishes at creation, forwarding to the local pod. Both sides authenticate with the node certificates the join flow already issues, and the source side keeps making all balancing and draining decisions, since it still picks the exact endpoint.

## Considered Options

1. **Published ports, plaintext** (research/overlay-wan.md's minimal step). Smallest change, and every cross-node byte crosses the network readable, with nothing checking who dialed in.
2. **Published ports with proxy-terminated mTLS** (chosen). The remote Envoy terminates TLS on its ingress listener, so the wire is encrypted and only certificate-holding nodes can connect. Costs one extra proxy hop on cross-node traffic.
3. **WireGuard mesh.** Still the recorded second step for when NAT traversal or flat pod IPs matter. Heavier: key lifecycle, MTU, rendezvous.
4. **Relay through klited.** Rejected outright — the control plane must never sit in the data path.

## Consequences

- The flat-bridge shortcut is gone everywhere. Locally the advertised address defaults to `host.docker.internal`, so the same code path (and the TLS handshake) runs in every demo instead of only in production.
- Donors publish a fixed ingress-port range at creation, because Docker can't add published ports to a running container. klited allocates per-endpoint ingress ports from that range.
- Agents advertise a routable machine address (flag, with the local default above). Mutual reachability is still required — a NAT'd node needs option 3.
- Identity is node-level, not per-service: any workload on a certified node can be dialed by any other node's proxy. The per-service SPIFFE-style upgrade stays future work, noted here so nobody mistakes the gap for an accident.
