# Everything instances must reach runs in the per-node infra pod

On macOS the host can't reach container IPs at all (the colima VM is a hard boundary), so the data path lives in containers. Each node runs a `klite-net` container that owns a network namespace and handles DNS on :53, VIP addresses via netlink, and TCP readiness probes. The Envoy container joins that namespace to bind the VIPs. The host-side rule is absolute: agents and klited touch containers only through the Docker API and 127.0.0.1-published ports, never by dialing 10.44/16.

```
node-1's infra pod (one shared netns)          instances on klite0
┌─────────────────────────────────┐
│ klite-net: dns :53, vips, probes│ ◀─DNS──── [a]
│ envoy:     listeners on VIPs    │ ◀─TCP───▶ [b-1] [b-2]
└─────────────────────────────────┘
        ▲ programmed by klite-agent (host, via Docker API + localhost ports)
```

## Considered Options

1. **Host-side data path.** Impossible here, and it would also quietly re-couple the design to "the control plane shares a machine with the instances."
2. **One combined image**, our binary plus Envoy in a single container. Fewer moving parts, but we'd be maintaining a fork of Envoy's image and marrying its upgrade cycle.
3. **Two containers sharing a network namespace** (chosen). Which is to say: we needed a pod, so we built one.

## Consequences

- Agent startup sequencing matters. klite-net comes first because it owns the netns, then Envoy joins with `network_mode: container:<klite-net>`.
- Everything netns-scoped rides the donor: the `host.docker.internal:host-gateway` mapping, NET_ADMIN, and the VIPs all go on klite-net's container, because Docker rejects those options on a container that joins another's network (spike 2 caught this live).
- Workload containers get `--dns <their node's klite-net IP> --dns-search svc.klite --dns-opt ndots:1`, a single upstream, because a second resolver racing NXDOMAINs corrupts name resolution in ways that only show under load. The ndots option is load-bearing: overriding DNS makes Docker write `ndots:0`, and musl resolvers then skip the search domain entirely, so bare names like `b` die on alpine images (spike 1 caught this).
- klite-net probes instance readiness from inside the network and reports it through the agent, since the host has no route to probe anything.
- That our infrastructure layer had to reinvent the pod is the best available argument for why Kubernetes has them. ADR 0014 records the irony.
