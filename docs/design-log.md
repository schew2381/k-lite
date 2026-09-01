# Design log

ADRs record what we decided, and this file records how we got there, one entry per design session, newest last. Everything below happened on 2026-08-31 and the day after.

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

A live-readiness audit then walked every UI affordance against the real backend and facade and turned up four break-on-day-one findings: the facade dialed plaintext into a TLS-only klited, `make ui` baked a mock bundle, protojson zero-value omission could crash pages on legal objects, and node controls only threw. We fixed or routed all four. The frontend now hardens its decoder, derives infra IPs from `nodeIndex` the way the server assigns them, hides controls the facade can't serve behind a capability field, and tags rollout stragglers from `templateHash`. In http mode it tells the truth: one klited card, heartbeat-driven stream pills, real `/klite/v1/` prefixes, wall-clock log stamps, and a stream-ended marker. The audit records the facade's own worklist (TLS plus bearer auth, scale/drain/uncordon routes, `?from=` watch resume, the traffic feed, and a status route) rather than guessing at it. The first two worklist items landed the same day: M8 relayed the exact break (a new Uncordon RPC orphaned the facade's test fake) and the fix pattern, so the facade now dials klited with the CA-pinned TLS and bearer token the CLI uses, resolved from flags, KLITE_CA/KLITE_TOKEN, or klited's state dir. With M8 declared done, the rest of the worklist landed except the traffic feed: the facade now serves scale (the CAS-safe RPC replaces the UI's read-modify-apply), drain (the progress stream bridged as chunked text), uncordon, and a node-token route that pairs the minted token with the klited endpoints. The UI's capabilities split cordon from uncordon, since live clusters only cordon through drain, and "add node" in http mode now finishes the story ADR 0018 starts: it declares the Node, then hands over the exact klite-agent join command, token included.

M9's cross-machine design reached the UI before its code reached main. The in-flight protos settle the shape (advertised machine addresses on Node status, mTLS ingress ports derived from a per-node window), so the mock adopted it wholesale: every node is its own machine on TEST-NET-2, EDS rows say local or internet from each node's viewpoint, and a remote traced call plays nine steps ending in the DNAT hop and the raw hand-off (ADR 0032). The writing gate caught the change's worst bug before landing, a quotation the proto never said, which had smuggled "pod" past the glossary into three trace strings. Commit hygiene arrived the same day: prek now runs a fast scoped hook set from the repo's own `prek.toml` (Biome fixes staged frontend files and gofmt only lists Go offenders), recorded in ADR 0031 after a live debate over which config format survives. Pacing then tightened around the stories: beats every four sim-seconds with the next story at most a couple behind the last, locality alternating local-then-internet by demo choreography, a lingering mTLS leg, and dots that fly straight lines only. The EDS box then became the actual dial table, one line per endpoint with the address this node's Envoy would use, and each infra pod grew an ingress box showing the reverse mapping remote proxies depend on: published mTLS port to raw local address. The prek record also got its correction after a sandbox verification confirmed the stash data-loss window: the shared checkout keeps the hook uninstalled, ADR 0031 now describes the mechanics honestly, and the gate is make precommit. A compatibility sweep against backend HEAD then verified the delete path end to end (generic deleteOne, orphan deletion by the workload controller, VIP release) and caught the backend materializing `IngressAllocation` as a stored kind, so the UI adopted it in both modes and ADR 0032's stream-only claim carries its correction. The header then collapsed to one segmented control that swaps the actual data source: picking live disposes the mock outright (its clock included) and connects the real client, so nothing simulates behind a live board.

## The build: ten milestones, decisions still landing

Design closed and the milestones ran: spikes (M0), the store and CLI (M1), agents and scheduling (M2), log streaming (M3), discovery (M4), drains and rollouts (M5), policy (M6), HA chaos (M7), the mTLS join surface (M8), and the cross-machine data plane (M9). Each one landed with an end-to-end harness under `hack/`, kept independently runnable so a regression in an old milestone fails a script with its number on it. The harnesses earned their pay in both directions, since verify-m9 alone ate twelve run-until-green iterations before its first clean pass.

The ADR habit held under implementation pressure. The build sessions recorded membership-by-YAML (0018), the moby client pick (0019), VIP allocations as a stored kind (0022), the deny-by-default RPC gate (0028), the iptables admin lockdown (0029), cluster identity labels (0030), the pending-delete label (0033), published-port mTLS ingress and its allocation kind (0034, 0035), dual-purpose node certs (0036), and revision-pinned deletes (0037), while the UI session minted 0023 through 0027 plus 0031 and 0032 in parallel. Two of those are reversals wearing supersession stamps. protoc gave way to buf the moment the proto surface grew more hands, which is the exact trigger ADR 0020 had named for its own retirement (0021), and M9 ended 0016's cross-host deferral on the two seams that record said would move (0034).

Parallel numbering left one scar worth keeping: 0023 and 0024 each got claimed twice, once per session, and the backend pair now lives at 0033 and 0034. INDEX.md maps both tracks rather than pretending the collision never happened.

## The reviews, and what they caught

Review ran as its own lane behind the build agents, adversarial on purpose: package-scoped code audits after M4 (netd, xds, ca, object, store, leader, policy), a controller audit after M5, an auth-and-runtime audit over the M8/M9 surface, and a writing critic over every batch of prose, per the CLAUDE.md mandate. The catches that justified the lane:

1. The VIP allocator could mint duplicate VIPs across leader lives, and two services sharing an address breaks that node's routing. Reconcile now reserves every address it sees and repairs contested ones.
2. Every write was revision-checked but deletes went by bare name, so a lagging leader could delete an instance that had already turned READY. `DeleteIfRevision` closed the hole and ADR 0037 records it.
3. Node certificates carried ClientAuth only, which broke the moment a destination Envoy presented one as an ingress server cert. Reproduced live with `fail_verify_error` climbing, fixed as ADR 0036.
4. netd's ApplyConfig could store an older config over a newer one when an agent retry raced a timed-out RPC, leaving stale DNS and VIPs the agent believed replaced. Pushes now serialize.
5. A forged YAML could nil-panic klited through the codec's envelope switch. The instance was fixed landing ADR 0035, then the class got closed: a parity test walks every kind through all four switches and a fuzz corpus holds the boundary.
6. One node's certificate could rewrite another node's instance statuses, or inject into its command streams. Both paths now bind to the authenticated node.

The writing lane pulled real weight too. Beyond style debts, it caught a fabricated proto quotation that had smuggled "pod" past the glossary into three UI trace strings, and doc comments describing diffs rather than the code they sat on.

## The process, multi-session

The build ran as several agent sessions sharing one `main`, committing directly and often. Coordination was written, not assumed: the frontend guessed the facade's contract behind a seam (0023), adopted the real dialect when it landed (0026), and took M9's shape from in-flight protos before the code reached main (0032). When M8's new Uncordon RPC broke the facade's test fake, the break and the fix pattern rode a session note, and the facade served the route the same day. Commit hygiene arrived mid-build as prek's fast staged-only hooks (0031) after a sandbox run proved the stash window could eat concurrent edits, which is why the shared checkout keeps the hook uninstalled and gates through `make precommit` instead.

The numbers the chaos harness prints make the architecture's case better than the diagrams do. Leadership moves about four and a half seconds after a SIGKILL (the 5s election lease is most of the wait), the churned store converges a couple of seconds later, and the client loop that runs through every drain, rollout, and leader kill finishes with zero failed requests, which was the bar the whiteboard set.
