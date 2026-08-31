# Cross-machine data plane: ports first, WireGuard second (the future ADR 0016 promised)

Sources: wireguard.com (protocol overview and the wg man pages), Tailscale's "How NAT traversal
works" post, nebula.defined.net, flannel's `Documentation/backends.md`, and docs.k3s.io on flannel
backends.

ADR 0016 deferred cross-machine traffic and promised this doc would record which future we'd reach
for. Its constraints still bind. Locally every instance shares one Docker bridge, and each
`Endpoint` already names its node (`api/proto/klite/v1/agent.proto:65`). Only two components may
change: the proxy's cross-node endpoint dialer and IPAM growing per-node subnets. The two
candidates below differ in where a packet crosses the machine boundary.

```
  OPTION A: dial the machine address             OPTION B: route instance IPs over the tunnel

  [a] ──▶ vip:80 (envoy on node-1)               [a] ──▶ vip:80 (envoy on node-1)
          │ picks node-2's endpoint                      │ picks node-2's endpoint
          ▼                                              ▼
  dial 198.51.100.7:30412 ──WAN──▶ node-2        dial 10.44.2.14:8080
  docker DNAT ──▶ [b] 10.44.2.14:8080                    │ kernel: 10.44.2.0/24 dev wg0
                                                         ▼
                                                 encrypted UDP ──▶ node-2's wg0 ──▶ [b]
```

## Option A: node-published ports

Each node publishes a host port per endpoint on its machine address, and remote proxies dial that
instead of the bridge IP. `Endpoint` grows a machine address and a published port beside the
`node` it already carries. The agent publishes instance ports through Docker the way it already
publishes klite-net's 127.0.0.1 admin port. The xDS layer renders endpoints from other nodes as
machine:port while local ones keep their bridge IPs, and that render branch is the entire dialer
change. A per-service port would be the NodePort shape and need a second balancer behind it, so
ports stay per-endpoint and balancing stays in the calling proxy. IPAM barely moves, since
per-node subnets matter here only for keeping `ip_identity` collision-free.

The costs are bookkeeping and honesty. Ports become a resource the control plane must allocate
from per-node ranges, persist, release when Instances die, and keep clear of whatever else the
machine listens on. Nothing is encrypted, and a published port answers anyone who can reach the
machine, which widens ADR 0009's accepted raw-IP bypass from one bridge to the whole network. NAT
is the sharpest edge. Agents dial out (ADR 0004), so a NATed node joins the cluster, heartbeats
status, and looks healthy. Every cross-node request aimed at it then blackholes, because joining
never proved anyone could dial in. Published ports fit nodes that already reach each other, one
LAN or one VPC, and the demo script should keep saying so out loud.

## Option B: a WireGuard mesh

Each node gets a wg interface and a keypair, and klited becomes the key distributor, since ADR
0013 already hands it an mTLS-authenticated channel to every agent. The agent mints its key at
join and registers the public half. klited streams each peer's public key, machine endpoint, and
instance subnet down `WatchDesired`, and the agent sets that subnet as the peer's `AllowedIPs`.
Cryptokey routing then does the dialer's work, since the kernel sends anything for 10.44.2.0/24
through node-2's peer and Envoy keeps dialing plain instance IPs. IPAM carries the real change,
carving 10.44.0.0/16 into per-node subnets, the second seam ADR 0016 named. Only registered keys
can put packets on the mesh, so the tunnel is encrypted and peer-authenticated by construction.

The mesh's costs are operational rather than architectural.

- Keys acquire a lifecycle. Minting at join is the easy half, while deleting a node must tear its
  peer entry out of every other node's table, which can ride the node registry the same way
  certificate revocation does in ADR 0013. Rotation needs a written story before anyone rotates.
- The tunnel eats MTU. Encapsulation adds 60 bytes over IPv4 and 80 over IPv6, which is why
  wg-quick pins a 1500 link's interface at 1420. A wrong value passes every small-packet test and
  then hangs large writes, while pings keep answering.
- Rendezvous isn't included. A WireGuard peer's endpoint is either configured or learned from the
  last authenticated packet, so a NATed peer must speak first and keep its mapping alive, and two
  peers both behind NAT never exchange that first packet.

That last gap is why every shipped WireGuard mesh grows a rendezvous layer. Tailscale runs DERP
relays (Detoured Encrypted Routing Protocol), which speak HTTP, carry traffic when hole punching
fails, and double as the side channel that coordinates the punching. Nebula elects
lighthouses, mesh members on reachable addresses that track where each host last appeared and
help peers punch toward each other. klited is already the one address every node dials, which
puts it in the lighthouse seat if k-lite ever needs one. k3s already ships the whole shape:
`--flannel-backend=wireguard-native` drives flannel's in-kernel backend, which gives each node a
`flannel-wg` interface on UDP 51820, mints the key into `/run/flannel/wgkey`, syncs public halves
through its subnet-lease metadata, and routes per-node pod subnets through the peers. The k3s
docs still warn the kernel module must exist on every node, the honest cost of a flag.

## The order ADR 0016 promised

We'd reach for published ports first, the WireGuard mesh second when encryption or NAT traversal
starts to matter, and never both at once. Ports are the smallest change that makes a two-machine
demo real, a proto field pair plus one allocator plus one branch in the snapshot renderer. They
also state their precondition plainly, which is that nodes already reach each other. When that
precondition fails, or the wire stops being trustworthy, the mesh replaces them and buys
encryption, peer authentication, and a NAT story at the price of key lifecycle, MTU care, and
rendezvous. Never run both, because each one is a complete answer to how a proxy reaches a remote
endpoint. Two answers would mean two dialers to debug, two failure modes per cross-node request,
and port bookkeeping the tunnel makes redundant. The day the mesh lands, the port allocator comes
out.

## What doesn't move either way

ADR 0016 listed these as the parts that wouldn't move, and both options keep that promise.

- DNS stays per-node and keeps answering with the asking node's own VIP (ADR 0006).
- VIPs stay node-local, so no VIP ever crosses a wire between machines.
- Policy still stands in the source node's Envoy RBAC (ADR 0009) before any cross-node hop starts.
- The agent protocol stays dial-out gRPC (ADR 0004), and `Endpoint.node` marks what's remote.
