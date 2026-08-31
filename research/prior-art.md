# Prior art: k3s, SwarmKit, Nomad

This note mines three shipped orchestrators for what a "lite" control plane kept, dropped, and
regretted, and names the mechanisms k-lite copies. Sources are shallow clones at
`~/code/{k3s,swarmkit,nomad}` (read 2026-08-31), cited by repo-relative path.

## k3s

k3s repackages Kubernetes as a single binary under 100 MB, keeping full conformance while
defaulting to sqlite storage and agents with zero inbound ports. It is the CNCF proof that "lite"
can mean packaging discipline rather than a forked API.

**One binary, upstream code.** The server process runs apiserver, scheduler, controller-manager,
and kubelet in-process behind an `Executor` interface (`pkg/daemons/executor/executor.go:31-47`).
Components are configured by synthesizing their own CLI flags as `[]string`
(`pkg/daemons/control/server.go:110-152`), so upstream's flag surface is the internal API and k3s
tracks upstream instead of patching it. The actual deletions are narrow. In-tree cloud providers
are compiled out with the `providerless` build tag (`scripts/build:66`) against a lightly-patched
fork (`go.mod` replaces `k8s.io/*` with `k3s-io/kubernetes v1.37.0-k3s1`), and a stub cloud
controller (`pkg/cloudprovider/`) fills the node-lifecycle hole. Everything else "removed" is a
bundling default, with containerd, flannel, CoreDNS, and Traefik shipping inside (README.md).

**remotedialer tunnel.** Every agent dials out over websocket to every server and holds the
connection open (`pkg/agent/tunnel/tunnel.go:367-392`, `remotedialer.ConnectToProxyWithDialer`).
The server end (`pkg/daemons/control/tunnel.go:35-40`) plugs into the apiserver egress selector,
so apiserver→kubelet traffic (exec, logs, metrics) rides the agent-initiated tunnel. It exists
for one reason the README states outright: workers never expose the kubelet port. Both ends
allowlist the loopback kubelet and stream ports (`pkg/agent/tunnel/tunnel.go:416`,
`pkg/daemons/control/tunnel.go:88`). This is the precedent for k-lite's dial-out-only rule, with
gRPC in place of websocket.

**Join token + CA pinning.** The token format is `K10<sha256 of CA bundle>::user:pass`
(`pkg/clientaccess/token.go:28,107-114`). `ParseAndValidateToken` (`token.go:127`) downloads the
server's CA bundle over TLS, hashes it, and compares against the pinned digest
(`token.go:154-193`) before trusting anything. A bare secret is upgraded to a K10 token with an
empty hash, making trust-on-first-use the explicit fallback (`token.go:219-226`). Kubeadm-style
expiring bootstrap tokens arrived later, for agents only (`docs/adrs/agent-join-token.md`),
because the original token doubles as the PBKDF2 passphrase for bootstrap-data encryption and
therefore can never rotate.

**kine.** The apiserver always speaks the etcd v3 gRPC API. Kine (`github.com/k3s-io/kine`, wired
in `pkg/cluster/cluster.go:169-207`) emulates that API over sqlite/MySQL/Postgres, or passes real
etcd endpoints through untouched. Even the serving cert names `kine.sock`
(`pkg/daemons/control/deps/deps.go:482`). k-lite runs real etcd and skips the translation layer,
but kine still marks the stable seam — the etcd API itself.

- Copy (transport): dial-out-only agents, with both tunnel ends allowlisting exact target ports.
- Copy (bootstrap): pin the CA hash inside the join token and verify it before first use.
- Copy (bootstrap): keep agent tokens separate from the server/root token, expirable and
  revocable from day one.
- Copy (control plane): one process, components driven through their existing public config
  surface, no private forks.
- Avoid (bootstrap): overloading the join token as an encryption passphrase. k3s's own ADR
  concedes a compromised server token means rebuilding the cluster.
- Avoid (transport): letting the tunnel grow routing duties. The egress-selector modes and
  pod/node CIDR tracking (`pkg/daemons/control/tunnel.go:103-160`) show the scope creep. k-lite's
  tunnel carries control traffic only.
- Skip (storage): kine's SQL backends buy edge portability k-lite doesn't need. Real etcd drops a
  moving part.

## SwarmKit

SwarmKit drives Docker Swarm mode. Managers replicate state over Raft and feed workers over gRPC,
while networking splits between central allocation and engine-side kernel programming.

**Transport.** `api/dispatcher.proto:21-64` defines the worker session. `Session` opens a
server-push stream, `Heartbeat` returns a server-chosen TTL, and `Assignments` streams desired
state where "the first message contains all of the tasks and secrets relevant to the node; future
messages are updates". That is snapshot-then-delta, the contract of k-lite's `WatchDesired`.
Control state is central Raft (`manager/state/raft/`). The only gossip in Docker Swarm lives in
engine-side libnetwork (networkdb) and distributes dataplane endpoint state, not orchestration
state.

**VIPs and the routing mesh.** A service gets one VIP per attached network, stored as
`Endpoint.VirtualIP{network_id, addr}` (`api/objects.proto:158-177`), with a per-service choice
of VIP vs DNS round-robin (`api/specs.proto:387-402`). The manager's allocator assigns VIPs and
ports centrally (`manager/allocator/networkallocator/networkallocator.go:57-65`) and hands each
node a per-node LB attachment IP (`networkallocator.go:82-88`). One overlay is flagged
`ingress: true` (`api/specs.proto:437-440`). The allocator allocates it before anything else, and
every service with `PublishMode: INGRESS` ports (`api/types.proto:691-702`) also gets a VIP on
that network (`manager/allocator/network.go:91-101,1181-1215`). Any node then accepts traffic on
a published port, DNATs into the ingress namespace, and kernel IPVS spreads it across task IPs
over VXLAN. None of that dataplane appears in SwarmKit (zero IPVS references in the repo). The
manager allocates numbers and the engine programs the kernel.

**Drain.** Node availability is a tri-state (ACTIVE, PAUSE, DRAIN), and DRAIN means "any task
already running will be evicted" (`api/specs.proto:31-44`), with no deadline, no rate limit, and
no per-service control.

- Copy (transport): snapshot-then-delta on one stream, heartbeat TTL assigned by the server.
- Copy (bootstrap): `SWMTKN-1-<base36 CA digest>-<secret>` (`ca/config.go:127,375`). A second
  system independently pinning the CA hash in the join token settles the design.
- Copy (VIPs): allocate addresses centrally in the store and program them locally, with a
  per-service DNS-RR escape hatch for clients that hold connections too long.
- Copy (drain): PAUSE as a distinct state is `klite cordon`. Cheap, and users expect it.
- Avoid (VIPs): kernel IPVS + VXLAN + gossip splits one user-visible failure ("service
  unreachable") across iptables, ipvsadm, overlay interfaces, and two repos. k-lite's per-node
  Envoy keeps the balancer in one userspace process with a config dump.
- Avoid (VIPs): a cluster-wide VIP forces every node to agree on backend state via gossip.
  Per-(service,node) VIPs owned by the local Envoy make each node's view independently correct
  and centrally distributed, with no dataplane consensus.
- Avoid (drain): eviction with no deadline or surge. SwarmKit's DRAIN is the cautionary baseline
  `klite drain` improves on.

## Nomad

Nomad pairs Raft servers with clients speaking msgpack RPC. There is no push channel. Clients
long-poll blocking queries pinned to Raft indexes.

**Desired-state distribution.** `client/client.go:2477+` (`watchAllocations`) runs the loop. A
blocking `Node.GetClientAllocs` returns a map of allocID → AllocModifyIndex. The client diffs
those indexes against its running allocs and fetches full bodies only for changed ones via a
second, stale-allowed `Alloc.GetAllocs` (`client.go:2589-2646`). Production taught it three
guards:

1. The first query demands consistency, later ones only monotonicity (`client.go:2505-2516`).
2. A response index at or below the request's min index is discarded as stale
   (`client.go:2578-2586`, issue #18267).
3. The second RPC may hit a lagging server and is retried on index regression
   (`client.go:2644-2660`).

k-lite's `WatchDesired` replaces the poll with a push stream but should keep the version-map diff
(send names and versions, fetch bodies on demand) and the resync-on-regression rule.

**Drain.** The API object is `DrainStrategy{DrainSpec{Deadline, IgnoreSystemJobs}, ForceDeadline}`
(`api/nodes.go:725-745`). Per-workload pacing lives in the job's `migrate` stanza, with
`max_parallel` (default 1), `health_check` ("checks"), `min_healthy_time` (10s), and
`healthy_deadline` (5m) (`api/tasks.go:388-400`), so operators set the deadline and workload
owners set the rate. The CLI (`command/node_drain.go:143-156`) exposes:

- `-deadline` (default 1h, `node_drain.go:20-22`), `-no-deadline`, `-force`
- `-ignore-system`, `-keep-ineligible`, `-self`, `-detach`, `-m <message>`, `-meta`
- `-monitor`, streaming per-alloc transitions ("marked for migration", "draining", status
  changes, then "Drain complete for node", `api/nodes.go:278,350-360`)

Server-side, deadlines sit in a coalescing heap (`nomad/drainer/drainer.go:33-43,82-84`) and
system jobs are skipped when ignored (`nomad/drainer/draining_node.go:114-117`). Draining also
marks the node scheduling-ineligible as a separate attribute, so a drain can finish while the
node stays cordoned.

- Copy (drain UX): the deadline triad (default 1h, `-no-deadline`, `-force`) plus `-monitor`
  streaming per-instance phases. `klite drain` should show surge → healthy → old-stopped.
- Copy (drain UX): `-m`/`-meta` audit fields stored on the node, and `-self` for the common case.
- Copy (drain semantics): eligibility (cordon) as separate state that outlives the drain, and
  `ignore_system_jobs` for node-agent-style workloads.
- Copy (transport): version-map diffing and index-regression resync inside `WatchDesired`.
- Avoid (drain): Nomad migrates stop-then-replace, rate-limited by replacement health
  (`nomad/drainer/watch_jobs.go`). k-lite's surge-first order should keep Nomad's pacing knobs
  but never drop below desired count.
- Avoid (transport): client-driven long-poll needs the three staleness guards above. A single
  ordered gRPC stream from one etcd watch avoids the cross-server index races.

## What all three agree on

- Desired state lives in one replicated store. Nodes hold caches, never truth.
- Nodes dial out, and two of the three need zero inbound ports on workers.
- Join tokens pin the CA digest (`K10...`, `SWMTKN-1-...`) until mTLS takes over.
- Every node-facing channel sends a full snapshot, then deltas, stream or long-poll.
- The server sets heartbeat TTLs and the client obeys them.
- Allocation is central, enforcement local. The node makes the store's numbers real.
- The "lite" that lasted came from packaging and defaults (k3s), not API surgery.
- Every drain design converges on a deadline, a per-workload rate, a system-job carve-out, and a
  cordon state distinct from the drain itself.
- A token that moonlights as an encryption key can never rotate. Design expiry in from day one.
- The shared regret is dataplane logic smeared across kernel, gossip, and a second repo (Swarm),
  or staleness guards bolted on after races shipped (Nomad, issue #18267). One owned, inspectable
  process per concern beats clever distribution.
