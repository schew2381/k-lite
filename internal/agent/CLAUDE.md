# internal/agent

The per-node agent. It registers with a token, watches desired state, and makes Docker match. Connections only ever go agent → server (ADR 0004): anything flowing back — logs, future exec — rides a stream the agent opened.

Invariants:

- The structure is independent long-lived loops (watch, events, reconcile, report, commands, plus infra-pod care). New long-running work copies an existing loop's reconnect-backoff shape and joins the shutdown WaitGroup.
- Reconcile is level-based against full snapshots. Orphan sweeps stay gated until the first snapshot arrives, so a fresh agent can't demolish a running node.
- The agent owns crash restarts (backoff 1s→30s) and the restart counter. dockerd never resurrects anything (ADR 0011).
- Stops honor the instance's termination grace as the docker stop timeout.
- The infra pod (klite-net donor + Envoy joining its netns) is agent-managed per ADR 0008. Sequencing matters: donor first, it owns the netns.
- SIGTERM just exits. Containers keep running and get adopted on the next run — that IS the contract.
- The node identity under `~/.klite/agent/<node>/tls` (join.go, ADR 0013) outlives the process: reuse it on every start, re-join only when the cluster rejects it (or it predates M9's server usage) AND a token is in hand. Envoy mounts the same directory for xDS and for the M9 ingress listeners, and loads the files once — identity content folds into the Envoy config hash so a re-join recreates the container.
- The donor publishes the node's whole ingress slice at creation (Docker can't add ports later; ADR 0024); the slice rides the config hash. --advertise-address resolves to a literal IP before it's reported — the donor's /etc/hosts first, host DNS second, loopback never (advertise.go).
- The command plane pins one klited per stream life (commands.go). Output pushes route only on the server holding the waiter, so never move them onto the round-robin channel.
