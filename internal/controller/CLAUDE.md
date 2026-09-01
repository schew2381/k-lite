# internal/controller

The leader-only reconcile loops: workload materialization, the scheduler, node lifecycle, and endpoints. klited runs them inside `leader.RunWhenLeader`, so at most one replica drives the cluster while every replica keeps serving the API.

Invariants:

- Loops are level-based (store watch plus periodic resync) and idempotent. A brief double-leadership window during failover must be harmless, which means CAS or create-only semantics on every write, deletes that are safe to repeat, and no in-memory state a rerun can't rebuild. Drain deadlines restart on a fresh leader, they never shorten.
- The scheduler filters (pin, Ready, not cordoned, capacity) then picks fewest-instances with a name tie-break (ADR 0012). Placement stays explainable in one sentence.
- Scale-down removes newest-first. Rollouts and drains follow the surge-first choreography with the DRAINING endpoint state (ADR 0010).
- Node death is two timers: NotReady after 15s of heartbeat silence, instances deleted for rescheduling 30s after that.
