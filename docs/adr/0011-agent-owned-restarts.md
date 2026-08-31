# The agent owns restarts, so Docker is told --restart=no

Workload containers run with Docker's restart policy disabled. The agent's reconcile loop detects exits, applies backoff, recreates, and counts. dockerd never resurrects anything on its own.

## Considered Options

1. **Docker restart policies.** Docker gives us free crash recovery, but it races the reconciler (both act on the same exit), hides restart state from the control plane, and resurrects containers the scheduler already moved elsewhere.
2. **Agent-owned lifecycle** (chosen). One owner per state transition, and restart counts become cluster state like everything else.

## Consequences

- The `RESTARTS` column in `klite get instances` is real data, not a guess reconstructed from Docker events.
- While an agent is down, its node's crashed containers stay down until it returns, which is precisely what NotReady detection and reschedule-after-grace exist to cover.
