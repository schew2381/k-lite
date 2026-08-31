# Draining is surge-first: capacity never dips

Removing a node and rolling a Workload share one choreography:

1. Cordon the node (or mark the old instance).
2. Create replacements elsewhere and wait for Ready.
3. Mark the old endpoints Draining, so they take no new connections while in-flight ones get `drainTimeout` (default 30s) to finish.
4. SIGTERM, wait `terminationGrace` (default 15s), SIGKILL.

A rolling update is the same dance one instance at a time. `klite drain <node>` and deleting a node's YAML both trigger it.

## Considered Options

1. **Drain-first, then recreate.** It matches the phrase "traffic turned off, then migrated" literally, but the service runs under-replicated in the gap. We keep it as the automatic fallback for when surviving nodes lack surge capacity.
2. **Kill-and-recreate.** The simplest thing, and it fails the zero-failed-requests requirement outright.
3. **Surge-first** (chosen). This is the k8s maxSurge=1/maxUnavailable=0 posture, and the verify scripts hold it to its promise: a client loop runs through every rollout and node drain asserting zero failed requests.

## Consequences

- Endpoints carry a state machine (Ready, then Draining, then gone) that flows through EDS, so Envoy stops picking Draining endpoints for new connections.
- Every Envoy cluster sets `healthy_panic_threshold: 0`. The default 50% panic mode ignores health the moment one endpoint of two drains, which would send fresh connections to the instance we're retiring (research/envoy-xds.md).
- The scheduler places surge instances with the draining node already excluded.
- The two timeouts live in Workload YAML under `drain:`, so a demo can shrink them for pace without touching code.
