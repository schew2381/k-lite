# Design log

ADRs record what we decided, and this file records how we got there, one entry per design session, newest last. Everything below happened on 2026-08-31.

## The brief

The whiteboard gave us five core requirements:

1. Everything is declared in YAML.
2. A server–client split exists from the start, with a CLI first and a UI on the same API later.
3. Workloads run on nodes, deployment-style.
4. Services discover each other by name, with real research behind whichever mechanism wins.
5. Everything runs local-first on Docker, behind interfaces generic enough that cloud nodes become a swap rather than a rewrite.

The stretch goals were internet-spanning nodes with authentication, a UI, visible mutation (scaling up and down, nodes joining and leaving), and network policies of the "A cannot talk to B" kind. The whiteboard also says PRESENTATION, TRADEOFFS, CHOICES, PROCESS. The decision trail is a deliverable.

Three choices came from direct Q&A before any design work started:

1. Go, over TypeScript, Python, and Rust (ADR 0002).
2. Build everything, rather than core-only.
3. Local nodes as agent processes sharing one Docker daemon (ADR 0003).

## The draft plan

The first architecture leaned minimal. It had a single server with bbolt, HTTP long-poll transport, a hand-written per-node DNS and L4 proxy, and ordered firewall rules for policy. A planning agent stress-tested the macOS data path and verified the two facts the whole design rests on. Docker's embedded DNS forwards unknown names to a per-container upstream, and static container IPs outside the dynamic range are safe to hand-manage. Both get re-proven by M0's spike script anyway, because a design this dependent on two facts shouldn't trust a document read.

## Grill round 1: five questions

Five questions drew five recommendations, and two survived.

| Question | Recommended | Chosen | Record |
|---|---|---|---|
| Discovery mechanism | in-house DNS + L4 proxy | reopened (how would an Envoy swap really work?) | ADR 0006, 0007 |
| Policy semantics | ordered firewall rules | reopened (how does Istio model this?) | ADR 0009 |
| Agent transport | plain HTTP long-poll + SSE | gRPC, for durability and istio/envoy fidelity | ADR 0004 |
| Storage and HA | bbolt, HA a non-goal | etcd, HA a goal | ADR 0005 |
| Vocabulary | keep k8s words | Workload, Service, Node, NetworkPolicy, with Pod and Deployment dropped | ADR 0001 |

The argument that carried the discovery discussion: adopting a proxy never buys you out of the resolution layer. Something of ours must still say what the name `b` means, so nginx, caddy, and traefik only outsource byte-forwarding, and the meshes that solve both halves require Kubernetes underneath.

## Grill round 2: the industrial pivot

Round 1's answers had a direction. Match how the real systems work, tolerate faults, drain gracefully. Round 2 followed it through:

- Envoy runs from day one instead of custom-proxy-first. The control plane becomes a miniature istiod speaking real xDS, and the custom proxy survives only as the spike-gated fallback (ADR 0007).
- etcd holds the state, klited copies go stateless, and controllers run single-active behind a lease election. The architecture converges on kube-apiserver and kube-controller-manager instead of diverging toward Nomad (ADR 0005, reversing round 1's bbolt).
- Policies take Istio's AuthorizationPolicy evaluation, scaled down. DENY wins, ALLOW flips a target to allowlist mode, and priority integers are gone (ADR 0009).
- Instance completes the vocabulary (ADR 0001).
- Draining goes surge-first, with drainTimeout at 30s and terminationGrace at 15s (ADR 0010).
- Scheduling spreads by count, with resources enforced only as cgroup limits (ADR 0012).
- Protos become the source of truth, with a hand-written facade for the browser (ADR 0004).

## Grill round 3: close-out

Node identity comes from a join token traded for an mTLS certificate (ADR 0013). The UI is fixed at four pages plus a policy simulator, because the policy evaluator is a pure function and exposing it costs one endpoint (ADR 0015). We also settled the recording rules themselves. Every decision gets an ADR when it's made, vocabulary changes update CONTEXT.md in the same commit, and each design session appends to this log (now mandated in CLAUDE.md).

## Process notes

The design ran as an interview in rounds. Questions came numbered, each with a recommendation, and the tree got recomputed after every answer. Facts were an agent's job, decisions the user's. A planning agent pre-verified the Docker DNS behaviors, and research agents attach evidence to ADRs 0005, 0007, 0009, and 0016 under `research/`. Adversarial writing critics review all repo prose against the `/writing` skill before it lands. Of the round-1 recommendations, two of five survived contact with the user, and the ADRs record both directions honestly.
