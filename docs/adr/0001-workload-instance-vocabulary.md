# Speak k-lite, not Kubernetes: Workload and Instance

We renamed the two central nouns instead of borrowing them. A **Workload** is what Kubernetes calls a Deployment, and an **Instance** is one running copy of it, where Kubernetes would say Pod. Service, Node, and NetworkPolicy keep their k8s names because our meanings stay close. The renamed pair marks exactly where our semantics diverge: an Instance is always one container, and a Workload is the only way to run one.

## Considered Options

1. **Keep the k8s vocabulary** (Deployment, Pod). It's instantly legible to anyone who knows Kubernetes, but every borrowed word imports promises we don't keep, like multi-container pods, namespaces, and selectors on everything. Each gap reads as a bug.
2. **Rename everything**, Services and Nodes included. Honesty at that dose is exhausting, because the audience spends the whole demo translating.
3. **Rename only where semantics diverge** (chosen). Workload and Instance are ours, and the rest stays familiar. We also say **VIP** and never ClusterIP, because our addresses are per-(Service, Node) rather than cluster-scoped.

## Consequences

- The CLI (`klite get workloads`), the API paths, the labels (`io.klite.workload`), and the container names all use the new nouns.
- `CONTEXT.md` is the source of truth, and its _Avoid_ lists are enforceable in review.
- A k8s-literate reader needs one pass through the glossary. After that, nothing lies to them.
