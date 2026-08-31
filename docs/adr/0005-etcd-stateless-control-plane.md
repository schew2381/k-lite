# etcd for state, klited stateless like kube-apiserver

Cluster state lives in a 3-member etcd. Every `klited` server handles reads and writes statelessly, and the controllers (scheduler, rollout, endpoints, nodes) run single-active behind an etcd lease election. It's the kube-apiserver and kube-controller-manager split at 1:100 scale. This reverses the draft decision to embed bbolt, which died the moment HA moved from non-goal to goal mid-design. docs/design-log.md tells that story.

## Considered Options

1. **bbolt embedded in a single klited.** This was the original pick while HA was a non-goal. It costs nothing to operate, lives in one file on disk, and can't be replicated.
2. **External etcd** (chosen). Consensus gets outsourced to the software Kubernetes itself trusts, watches and lease elections come included, and the architecture converges on the real thing instead of diverging from it.
3. **Embedded raft (hashicorp/raft).** This is the Consul and Nomad shape, self-contained and impressive. Raft integration (snapshots, membership changes, split-brain edges) is also the largest correctness tarpit this project could buy.
4. **Postgres behind a kine-style shim.** It gets HA by delegating to a database we'd also have to run, and it teaches the least.

## Consequences

- `hack/etcd-up.sh` runs three etcd containers with client ports on 127.0.0.1, so the cluster has infrastructure of its own before the first node joins.
- Any klited can die without losing data. M7's kill-the-leader demo exists because of this decision.
- Writes use mod-revision transactions, and controllers stay level-based and idempotent so a brief double-leadership window is harmless.
