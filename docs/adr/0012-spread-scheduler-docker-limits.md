# Scheduling spreads by count, and resources are limits, not placement

The scheduler filters (node pin honored, node Ready, not cordoned, under `maxInstances`) and then picks the node running the fewest instances. Workloads may declare `resources:` (cpus, memory), and those become Docker cgroup limits on the container. They play no part in placement.

## Considered Options

1. **Resource-aware bin-packing.** That's the real k8s scheduler's job, and it's invisible on a single Mac where every "node" shares the same physical cores anyway.
2. **Ignore resources entirely.** Simpler, but then the YAML can't express limits Docker readily enforces.
3. **Spread plus enforced-but-not-scheduled resources** (chosen). Placement stays explainable in one sentence, which a demo needs more than optimality, while limits stay real at the cgroup level.

## Consequences

- Every scheduling decision has a one-line explanation, and `klite describe instance` can print it.
- If genuinely heterogeneous nodes ever join, this is the ADR to supersede.
