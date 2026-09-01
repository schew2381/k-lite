# internal/agent

The per-node agent. It registers with a token, watches desired state, and makes Docker match. Connections only ever go agent → server (ADR 0004): anything flowing back — logs, future exec — rides a stream the agent opened.

Invariants:

- The structure is independent long-lived loops (watch, events, reconcile, report, commands, plus infra-pod care). New long-running work copies an existing loop's reconnect-backoff shape and joins the shutdown WaitGroup.
- Reconcile is level-based against full snapshots. Orphan sweeps stay gated until the first snapshot arrives, so a fresh agent can't demolish a running node.
- The agent owns crash restarts (backoff 1s→30s) and the restart counter. dockerd never resurrects anything (ADR 0011).
- Stops honor the instance's termination grace as the docker stop timeout.
- The infra pod (klite-net donor + Envoy joining its netns) is agent-managed per ADR 0008. Sequencing matters: donor first, it owns the netns.
- SIGTERM just exits. Containers keep running and get adopted on the next run — that IS the contract.
