---
status: superseded by 0034-cross-machine-published-ports-mtls
---

# Cross-host traffic is designed for, not built

Locally every instance shares one bridge, so a proxy on node-1 dials an endpoint on node-2 directly. Once nodes run on separate machines, that dial breaks, and we're deliberately not building the fix yet. The seams already fit it: EDS endpoints carry their node, so the change is confined to the proxy's cross-node endpoint dialer and to IPAM growing per-node subnets. DNS, VIPs, policy evaluation, and the agent protocol wouldn't move.

## Considered Options

1. **WireGuard overlay now.** It's real and encrypted, but it's an entire subsystem (key distribution, MTU, NAT traversal) for a scenario no local demo can exercise.
2. **Node-published ports now.** It's cheaper than an overlay and still dead code locally.
3. **Defer, with the seams named** (chosen). `research/overlay-wan.md` records which future we'd reach for first (published ports, with the overlay as the second step).

## Consequences

- Demos state the flat-bridge shortcut out loud instead of hiding it.
- Whoever adds multi-machine networking starts from a named list of the two things that change, not an archaeology dig.
