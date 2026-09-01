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

## The frontend, mock-first

The UI couldn't wait for klited, so it runs on a simulator. `frontend/` holds a React app whose only backend is a `KliteClient` interface, with a headless TypeScript mini control plane behind it honoring the drain, restart, scheduling, and policy ADRs (0023). The animated call view needed per-connection events the frozen facade never carried, so ADR 0024 reopens 0015 with one SSE route, `GET /v1/traffic`, exactly as 0015 said such a wish must.

Live review reshaped the traffic model: we slowed calls to one beat per five seconds (slowed again later, ADR 0025), kept at most two callers on distinct nodes, and gave each dot a route label. We also renamed the directories: the app top-level is `frontend/` (not `ui/`) and its source `frontend/app/` (not `src/`), because a repo holding Go and TypeScript shouldn't have a directory whose name says nothing about which half it is. Once the backend's protos landed on main, we re-cut the frontend types against them, down to `status.unschedulable` and integer drain seconds.

One bug earned a regression suite of its own: a MODIFIED emitted after a node's DELETED resurrected the card in every watch consumer while the cluster map looked correct (ADR 0023 records the fix). The watch replay is now tested as its own layer.

## The UI grows an etcd browser and request traces

Two more review asks landed while the frontend ran: show what etcd holds, and make a single request legible. The etcd page browses every key with live mod revisions, and the topology plays one traced call at a time (resolver, kdns, listener, RBAC, endpoint), with the dot moving exactly when packets move (ADR 0025, which supersedes 0015's four-page freeze). We validated the trace sentences against ADRs 0006 through 0017 rather than trusting the diagram that inspired them. An adversarial code review also ran before this landed. It proved two simulator bugs with scripts (task-key prefix collisions stranding double-digit instances, and node revival resurrecting draining ones), and both got regression tests. The same review produced a real-time tick driver, exact-key cancellation, and a shared endpoint-state helper.

A later pass rebuilt the story around what actually moves. The infra pod split into kdns, listeners, RBAC, and endpoints sub-boxes that the traced dot visits in turn, and the animation now plays DNS as a real round trip before a separate dial, because the first cut wrongly showed the query answered in place (ADR 0025 records the verification against ADRs 0006 through 0017). A second adversarial round reviewed code, architecture, and prose together. It surfaced four HIGH findings, all fixed: a reduced-motion trace that deadlocked after unmount, a board-width observer that never attached in http mode, reconnect semantics that contradicted the wire contract, and a node whose removal never finished once its agent died.

## The frontend meets the real facade

`internal/facade` landed in the backend's working tree, and it settled every question ADR 0023's guessed contract had left open, mostly by answering differently. Routes sit under `/api`. Lists speak the codec's user-facing JSON while watch events are raw protojson, so we grew a decoder that normalizes both into one canonical shape. The Watch RPC delivers changes only, which makes the client's life list-then-watch with a reset on every reconnect. VIPs stopped being a Service decoration: ADR 0022's server-materialized `VIPAllocation` kind is now the only source, the mock reconciles those objects with the same create-heal-release behavior as the real leader controller, and the etcd browser gained the `/klite/vipallocations` prefix. Policy checks answer an availability envelope until klited implements the RPC, with the mock wording its verdicts exactly as `internal/policy` does. ADR 0026 records the alignment and its one known gap, the list-to-watch race that closes when the facade exposes `from_revision`. The traced walkthrough also stopped pretending it could survive contact with real traffic. ADR 0027 splits the dot layer into a traced flow at the mock's teaching pace and a live flow that plays every call in about two seconds while the panel holds the latest path. The flow keys to the client mode, with a `?flow=` override for previews.

A live-readiness audit then walked every UI affordance against the real backend and facade and turned up four break-on-day-one findings: the facade dialed plaintext into a TLS-only klited, `make ui` baked a mock bundle, protojson zero-value omission could crash pages on legal objects, and node controls only threw. We fixed or routed all four. The frontend now hardens its decoder, derives infra IPs from `nodeIndex` the way the server assigns them, hides controls the facade can't serve behind a capability field, and tags rollout stragglers from `templateHash`. In http mode it tells the truth: one klited card, heartbeat-driven stream pills, real `/klite/v1/` prefixes, wall-clock log stamps, and a stream-ended marker. The audit records the facade's own worklist (TLS plus bearer auth, scale/drain/uncordon routes, `?from=` watch resume, the traffic feed, and a status route) rather than guessing at it.
