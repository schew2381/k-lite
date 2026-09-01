# hack/

The operational scripts fall into three families:

- `verify-m*.sh` — the e2e layer, one per milestone. Convention: PASS/FAIL echoed per step, nonzero exit on any required failure, cleanup that leaves shared fixtures (etcd, the klite0 network, images) in place. They must stay independently runnable, so regressions get caught by re-running earlier ones.
- `demo.sh` / `dev-up.sh` / `dev-down.sh` / `bootstrap.sh` — the presentation run and the developer playground: one command to a narrated full demo or a quiet running cluster, one to tear either down (pidfile-verified kills, label-scoped container removal), one to set up a fresh machine.
- `spike-*.sh` / `spike-envoy/` — the M0 evidence gates behind ADRs 0006–0008. They're historical: keep them runnable and don't extend them.

When several stacks share the Docker daemon (parallel agents, playground beside a verify run), isolate by overriding ports, node names, and the etcd prefix (`ETCD_NAME_PREFIX`/`ETCD_PORT_BASE`/`ETCD_NET`), and scope every cleanup to names you created.
