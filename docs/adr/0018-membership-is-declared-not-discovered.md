# A node registers only if its YAML already exists

Agent registration is rejected unless a Node object with that name is already in the store. Cluster membership is therefore declared twice, on purpose: the YAML says the node *should* exist, and the join token proves the machine asking is allowed to *be* it. Deleting the YAML is how a node leaves.

## Considered Options

1. **Auto-register on first contact** (what kubelet does). It saves one step, but then "apply a per-node YAML file" stops being the membership story the project promised, and any process holding the shared token could mint nodes.
2. **Registration requires the declared Node object** (chosen). `klite apply -f nodes/node-4.yaml` then starting the agent is the whole join flow, and the declarative model stays honest.

## Consequences

- Node add and remove demos are pure YAML operations, which is the point.
- A typo in `--node` yields a clear "apply the node YAML first" error instead of a ghost node.
- Per-node join tokens (ADR 0013) get a natural anchor: the token binds to a declared name.
