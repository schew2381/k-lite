# internal/server

klited's gRPC surface: ClusterService for clients, AgentService for nodes, ADS for Envoys, and the command hub bridging log streams. Every replica runs all of this — statelessness is the point (ADR 0005).

Invariants:

- No state that can't be rebuilt from an etcd watch. In-memory structures (command hub, per-node net snapshots) are caches a fresh replica reconstructs.
- Anything that must run once cluster-wide belongs in `controller` under the leader lease, not here.
- Writes are CAS-with-retry through the store. Never read-modify-write without the revision.
- Registration requires the declared Node object (ADR 0018) and validates the cluster token — or accepts an existing node cert for the same name.
- Every RPC passes auth.go's deny-by-default gate (ADR 0013): node certs open AgentService and xDS, the admin bearer opens ClusterService, Register alone is public. New services must claim a caller class there or stay unreachable. xDS streams are additionally bound to the certificate's node (M9, closing ADR 0028's residual): the first request must name it and a mid-stream rename dies.
- Per-node streams (WatchDesired, commands) resend a full snapshot after any watch error — compaction is normal, dying on it is not.
- The server never dials an agent. If a feature seems to need that, it actually needs a command on the agent's stream.
