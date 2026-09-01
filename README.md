# k-lite

k-lite is a small Kubernetes-like orchestrator built from scratch to understand why the real one is shaped the way it is. Declarative YAML goes in, scheduled containers come out, and discovery, rolling updates, network policy, and cross-machine mTLS all run on plain Docker. The control plane is a miniature kube-apiserver and istiod: etcd holds the state, stateless `klited` replicas serve gRPC and xDS, and an upstream Envoy on every node moves the actual bytes.

## Quickstart

```
make bootstrap   # brew tools, colima, base images (safe to re-run)
make demo        # fresh cluster, every headline feature, ends on the live board
```

The demo wipes any previous playground, boots etcd and two klited replicas, joins three nodes over mTLS, and then walks through discovery, a live scale, a hitless rollout, a policy denial, a node drain, and a leader kill, gating each beat PASS/FAIL. It ends by opening the web board against the running cluster and leaves everything up to poke at. `hack/dev-up.sh` is the quieter alternative (same cluster, no narration), and `hack/dev-down.sh --all` tears either down.

## Architecture

```
                    CONTROL PLANE
   ┌───────────────────────────────────────────────┐
   │        etcd-1 ── etcd-2 ── etcd-3             │
   │                    ▲                          │
   │      klited A ─────┴───── klited B            │
   │      (leader)             (standby)           │
   │  stateless; controllers single-active         │
   │  behind an etcd lease election                │
   └───────▲───────────────▲───────────────▲───────┘
      gRPC │          gRPC │ mTLS      xDS │ mTLS
           │               │               │
      klite CLI,      klite-agent     Envoy, per node
      facade + UI     (dials out)     (dials out)
```

Nothing ever dials into a node. Agents and Envoys hold outbound streams to any klited replica, which is what lets a node behind NAT join a WAN cluster (ADR 0004) and lets any replica die without ceremony (ADR 0005).

Each node is one agent process locally and would be one real machine later, same binary either way:

```
   ONE NODE
   ┌─────────────────────────────────────────────────┐
   │  klite-agent (host process, Docker API only)    │
   │                                                 │
   │  infra pod ─ one shared netns (ADR 0008)        │
   │  ┌───────────────────────────────────────────┐  │
   │  │ klite-net: kdns :53, VIPs, probes         │  │
   │  │ envoy:     VIP listeners, RBAC, EDS,      │  │
   │  │            mTLS ingress listeners         │  │
   │  └───────────────────────────────────────────┘  │
   │                                                 │
   │  workload containers, one per Instance          │
   └─────────────────────────────────────────────────┘
```

Traffic has two legs. When `a` calls `b` and the picked endpoint is local, the whole path stays on the node:

```
   [a] ──1 DNS──▶ kdns ──2 VIP answer──▶ [a] ──3 dial──▶ envoy ──4 raw──▶ [b, same node]
```

When the pick is on another machine, the source Envoy crosses the open internet to the owner's published ingress port, mTLS with node certificates on both ends, and the owning Envoy hands off raw locally (ADRs 0034–0036):

```
   [a] ──▶ envoy A ════ mTLS, node certs ════▶ addr:20017 ──▶ envoy B ──raw──▶ [b, node B]
            picks the endpoint                 published        terminates
            and balances                       ingress port     TLS, forwards
```

Locally the advertised address defaults to `host.docker.internal`, so every demo runs the same handshake production would.

## What it does, and the script that proves it

Every milestone kept an end-to-end harness, and they stay independently runnable:

| Capability                                                                | Proof               |
| ------------------------------------------------------------------------ | ------------------- |
| YAML apply/get/scale/delete, etcd store, replica and etcd-member kills    | `hack/verify-m1.sh` |
| Scheduling across agents, container kill and dead-node recovery           | `hack/verify-m2.sh` |
| Log tail/follow and describe                                              | `hack/verify-m3.sh` |
| Discovery end to end: kdns, VIPs, probe-gated READY, xDS load balancing   | `hack/verify-m4.sh` |
| Surge-first rollouts and drains with zero failed requests                 | `hack/verify-m5.sh` |
| NetworkPolicy, data plane and `klite policy check` never disagreeing      | `hack/verify-m6.sh` |
| HA chaos: leader SIGKILL mid-churn, etcd member down, agent kill          | `hack/verify-m7.sh` |
| mTLS joins, deny-by-default auth, admin lockdown, a WAN-shaped join       | `hack/verify-m8.sh` |
| Cross-machine mTLS ingress: allocations, hitless churn, plaintext refused | `hack/verify-m9.sh` |

The build gates are `make test` (vet + `-race -shuffle=on`), `make lint`, and `make proto-lint`, and CI runs all of them plus `govulncheck` on every push.

## Seeing it

- [`frontend/`](frontend/README.md) is the live board: topology with traced calls, an etcd browser, tables, logs, policies. `make demo` opens it wired to the real cluster, and its MOCK mode runs a full in-browser simulator of the same ADRs (ADR 0023).
- [`docs/design.html`](docs/design.html) is the interactive design walkthrough from before any code existed.

## The decision trail

The decision trail is a deliverable here, not a byproduct:

- [`CONTEXT.md`](CONTEXT.md) holds the glossary and says why a Workload is not a Deployment and an Instance is not a Pod.
- [`docs/adr/`](docs/adr/) keeps one record per decision, rejected options and tradeoffs included, with [`INDEX.md`](docs/adr/INDEX.md) as the map. One numbering scar is left visible: the backend and frontend sessions each minted a 0023 and 0024 while landing in parallel, and the backend pair now lives at 0033 and 0034.
- [`docs/design-log.md`](docs/design-log.md) tells how the sessions actually went, reversals included.
- [`research/`](research/) collects the tool-by-tool evidence behind the big choices.
