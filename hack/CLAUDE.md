# hack/

Operational scripts. Three families:

- `verify-m*.sh` — the e2e layer, one per milestone. Convention: PASS/FAIL echoed per step, nonzero exit on any required failure, cleanup that leaves shared fixtures (etcd, the klite0 network, images) in place. They must stay independently runnable — regressions get caught by re-running earlier ones.
- `dev-up.sh` / `dev-down.sh` / `bootstrap.sh` — the developer playground: one command to a running cluster with example apps, one to tear it down (pidfile-verified kills, label-scoped container removal), one to set up a fresh machine.
- `spike-*.sh` / `spike-envoy/` — the M0 evidence gates behind ADRs 0006–0008. Historical, keep runnable, don't extend.

When several stacks share the Docker daemon (parallel agents, playground beside a verify run), isolate by overriding ports, node names, and the etcd prefix (`ETCD_NAME_PREFIX`/`ETCD_PORT_BASE`/`ETCD_NET`), and scope every cleanup to names you created.
