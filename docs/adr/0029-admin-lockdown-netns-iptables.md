# Admin ports get iptables in the donor netns, not authentication

klite-net's admin gRPC (:9090) and Envoy's admin (:9901) were reachable from every workload container on the bridge — an unauthenticated tenant could rewrite a node's DNS or kill its proxy. The fix is a run-once helper container (alpine with iptables, NET_ADMIN, joining the donor's netns) that drops those ports for the VIP and instance source ranges. The docker-proxy path dials from the bridge gateway (10.44.0.1, verified empirically), so the host's published-port access keeps working.

## Considered Options

1. **mTLS on the admin APIs.** Right for klite-net's gRPC eventually, but Envoy's admin endpoint can't demand client certs, so half the problem survives and klite-net's scratch image grows credential plumbing.
2. **Stop publishing the Envoy admin port.** Closes the host's debugging window (`config_dump` is the primary Envoy debugging tool) and does nothing about bridge-side access to :9090.
3. **Netfilter rules in the netns the ports live in** (chosen). One helper, both ports, workloads blocked, host path intact.

## Consequences

- Rules apply idempotently (check-then-insert), surviving agent restarts and re-pushes.
- A workload granted CAP_NET_RAW could still spoof SYNs from allowed sources but can't complete a handshake, so the residual is noise, not access.
- The helper image is the infra pod's only non-scratch dependency, pulled once per machine.
