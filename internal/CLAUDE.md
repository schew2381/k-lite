# internal/ package map

One line per package so you land in the right place. Packages with real invariants carry their own CLAUDE.md.

- `object` — YAML ↔ proto codec, validation, defaulting, template hashing.
- `store` — the Store interface and its etcd implementation, the only path to cluster state.
- `leader` — etcd lease election wrapper. Controllers run inside `RunWhenLeader`.
- `server` — klited's gRPC surface: ClusterService, AgentService, ADS wiring, command hub.
- `controller` — leader-only reconcile loops: workloads, scheduler, nodes, endpoints.
- `runtime` — the Runtime interface plus its Docker implementation.
- `agent` — klite-agent's loops: register, watch, reconcile, report, commands, infra pod.
- `netd` — the klite-net daemon that runs inside the per-node infra-pod container.
- `xds` — per-node NetDesired → Envoy ADS snapshots.
- `policy` — the pure istio-lite policy evaluator.
- `ca` — certificate authority, join tokens, TLS config builders.
- `cli` — cobra commands behind the `klite` binary.
- `gen` — generated protobuf/gRPC code. Never edit by hand.
- `facade` — OWNED BY THE USER'S SEPARATE SESSION. Do not modify, review, or commit anything here, in `cmd/klite-facade`, in `frontend/`, or in the Makefile `ui` target.

Identifiers and output speak CONTEXT.md's vocabulary (Workload, Instance, Service, Node — never Pod or Deployment). Architectural boundaries have ADRs in `docs/adr/`. Read the relevant one before moving a boundary, and record a new ADR when you do.
