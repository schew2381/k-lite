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

## Outcome

verify-m7 puts numbers on the promise. With the workload mid-scale-churn, the standby logged `controllers: leading` 4.5s after the leader's SIGKILL (the 5s election-lease TTL is most of that wait) and converged the churned store to 8/8 within another 2s, with no duplicate or orphaned containers. No node dipped from Ready through the takeover. The same run survives an etcd member stop without a visible wobble, recovers from full quorum loss once members return (a leader re-establishes about 7s after restore), and resumes a mid-flight rollout under a second leader kill with the instance count held in [3,5]. The kill-the-leader demo this ADR predicted is now one beat of `make demo`.
