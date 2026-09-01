# ADR index

Each record gets one line. The track column says where a decision came from: the design sessions before any code (design), the backend build sessions (build), or the UI session that ran beside them (ui). The two tracks numbered in parallel and collided once, so 0023 and 0024 were each claimed twice. The frontend records kept those numbers, the backend pair moved to 0033 and 0034, and the history keeps the collision visible instead of rewriting it.

| ADR                                                | Decision                                                    | Track  | Status                                                          |
| -------------------------------------------------- | ----------------------------------------------------------- | ------ | --------------------------------------------------------------- |
| [0001](0001-workload-instance-vocabulary.md)        | Speak k-lite, not Kubernetes: Workload and Instance          | design | accepted                                                         |
| [0002](0002-go-and-four-binaries.md)                | Go, one module, four binaries                                | design | accepted                                                         |
| [0003](0003-local-nodes-as-agent-processes.md)      | A local node is an agent process, not a VM                   | design | accepted                                                         |
| [0004](0004-grpc-everywhere-agents-dial-out.md)     | gRPC everywhere, and agents always dial out                  | design | accepted                                                         |
| [0005](0005-etcd-stateless-control-plane.md)        | etcd for state, klited stateless like kube-apiserver         | design | accepted, outcome recorded                                       |
| [0006](0006-dns-plus-per-service-node-vips.md)      | Discovery: our own DNS, and a VIP per (Service, Node)        | design | accepted                                                         |
| [0007](0007-envoy-xds-from-day-one.md)              | Envoy is the data plane from day one, speaking real xDS      | design | accepted, spike gate passed                                      |
| [0008](0008-per-node-infra-pod.md)                  | Everything instances must reach runs in the infra pod        | design | accepted                                                         |
| [0009](0009-istio-lite-policy-semantics.md)         | NetworkPolicy speaks Istio's model, scaled down              | design | accepted                                                         |
| [0010](0010-surge-first-drain.md)                   | Draining is surge-first: capacity never dips                 | design | accepted, outcome recorded                                       |
| [0011](0011-agent-owned-restarts.md)                | The agent owns restarts, Docker told --restart=no            | design | accepted                                                         |
| [0012](0012-spread-scheduler-docker-limits.md)      | Scheduling spreads by count, resources are limits            | design | accepted                                                         |
| [0013](0013-mtls-node-identity.md)                  | Nodes join with a token, then hold an mTLS certificate       | design | accepted, outcome recorded                                       |
| [0014](0014-one-container-per-instance.md)          | One container per Instance, for now                          | design | accepted                                                         |
| [0015](0015-ui-four-pages-plus-simulator.md)        | The UI is four pages and a policy simulator                  | design | superseded in part by [0024](0024-traffic-feed-reopens-0015.md) and [0025](0025-etcd-browser-and-request-traces-in-the-ui.md), ownership moved to the ui track |
| [0016](0016-cross-host-data-plane-deferred.md)      | Cross-host traffic is designed for, not built                | design | superseded by [0034](0034-cross-machine-published-ports-mtls.md) |
| [0017](0017-dns-zone-and-ttl.md)                    | Names live under svc.klite, TTL 5s, never deny existing      | design | accepted                                                         |
| [0018](0018-membership-is-declared-not-discovered.md) | A node registers only if its YAML already exists           | build  | accepted                                                         |
| [0019](0019-moby-client-module.md)                  | Docker SDK: moby/moby/client, not the frozen docker/docker   | build  | accepted                                                         |
| [0020](0020-protoc-over-buf.md)                     | Codegen runs on protoc, despite the research                 | build  | superseded by [0021](0021-buf-toolchain.md)                      |
| [0021](0021-buf-toolchain.md)                       | Proto tooling moves to buf                                   | build  | accepted, supersedes 0020                                        |
| [0022](0022-vip-allocations-as-a-kind.md)           | VIP allocations are a first-class stored kind                | build  | accepted                                                         |
| [0023](0023-mock-first-frontend-behind-a-client-seam.md) | The frontend ships before the backend, behind a client seam | ui | accepted                                                        |
| [0024](0024-traffic-feed-reopens-0015.md)           | A live traffic feed reopens 0015's frozen facade             | ui     | accepted, reopens 0015                                           |
| [0025](0025-etcd-browser-and-request-traces-in-the-ui.md) | The UI gains an etcd browser and step-traced requests  | ui     | accepted, supersedes 0015's page count                           |
| [0026](0026-frontend-adopts-the-facade-dialect.md)  | The frontend adopts the facade's real dialect                | ui     | accepted                                                         |
| [0027](0027-traced-flow-teaches-live-flow-reflects.md) | Two traffic flows: traced teaches, live reflects          | ui     | accepted                                                         |
| [0028](0028-deny-by-default-rpc-gate.md)            | One TLS listener, every RPC passes a deny-by-default gate    | build  | accepted                                                         |
| [0029](0029-admin-lockdown-netns-iptables.md)       | Admin ports get iptables in the donor netns                  | build  | accepted                                                         |
| [0030](0030-cluster-identity-guards.md)             | Infra containers carry a cluster identity                    | build  | accepted                                                         |
| [0031](0031-prek-pre-commit-hooks.md)               | prek runs staged-only commit hooks that never rewrite Go     | ui     | accepted                                                         |
| [0032](0032-the-ui-shows-the-internet-leg.md)       | The UI shows the internet leg before M9 serves it            | ui     | accepted, amended by [0035](0035-ingress-allocations-as-a-kind.md) |
| [0033](0033-pending-delete-label.md)                | Node deletion is a label, re-applying the YAML cancels it    | build  | accepted, outcome recorded                                       |
| [0034](0034-cross-machine-published-ports-mtls.md)  | Cross-machine traffic rides published ports behind proxy mTLS | build | accepted, outcome recorded, ends 0016's deferral                 |
| [0035](0035-ingress-allocations-as-a-kind.md)       | Ingress ports are allocations, listeners follow allocations  | build  | accepted, outcome recorded                                       |
| [0036](0036-dual-purpose-node-certs.md)             | Node certificates carry both TLS purposes                    | build  | accepted, outcome recorded                                       |
| [0037](0037-revision-pinned-deletes.md)             | Deletes carry the revision they acted on                     | build  | accepted                                                         |
| [0038](0038-releases-carry-the-join-path.md)        | Real machines join from GitHub Releases, donor image on the wire | build | accepted                                                        |
| [0039](0039-chatty-apps-deterministic-probers.md)   | Demo apps chatter, verify gates run their own probers | build | accepted                                                        |
| [0040](0040-one-click-local-joins.md)              | The facade starts local agents for one-click joins | build | accepted                                                        |
| [0041](0041-traffic-feed-from-envoy-counters.md)   | The traffic feed streams Envoy counter deltas | build | accepted                                                        |
| [0042](0042-apply-preserves-server-status.md)      | Apply can't write status, heartbeats restore a lost node index | build | accepted                                                        |
| [0043](0043-internet-clusters-ride-an-overlay.md)  | Internet clusters ride an overlay, not port maps | build | accepted                                                        |
| [0044](0044-kdns-attributes-callers.md)            | kdns attributes callers for the traffic feed | build | accepted                                                        |
| [0045](0045-dev-down-all-forgets-the-cluster.md)   | dev-down --all forgets the cluster | build | accepted                                                        |
| [0046](0046-the-playground-serves-the-board.md)    | The playground serves the board | build | accepted                                                        |
| [0047](0047-the-agent-earns-a-face.md)             | The agent earns a face on the node card | build | accepted                                                        |
| [0048](0048-chatty-waves-stretch-to-ten-seconds.md) | Chatty waves stretch to ten seconds | build | accepted                                                        |
| [0049](0049-advertise-rides-the-overlay.md)        | The advertise address rides the overlay when one exists | build | accepted                                                        |
| [0050](0050-instance-logs-name-their-direction.md) | Instance logs name their direction | build | accepted                                                        |
