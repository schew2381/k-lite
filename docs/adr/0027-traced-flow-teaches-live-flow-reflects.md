# Two traffic flows: traced teaches, live reflects

A real cluster can't pause the world for a 27-second walkthrough of every request, and the mock's teaching pace would misrepresent live traffic anyway. The dot layer now has two flows. Traced flow keeps today's behavior of one call at a time, with long holds at each step, a numbered label on the dot, and the trace panel stepping in sync. Live flow plays every call as a fast flight over the same full path (resolve, kdns round trip, dial, RBAC, EDS pick, relay) in about two seconds, carrying only its route label. The trace panel holds the latest call's whole story instead of stepping. The flow follows the client mode (mock gets traced and http gets live), and a test hook overrides it for previews.

## Considered Options

1. **One pacing everywhere.** Live traffic at teaching pace means the board shows one request per half minute while hundreds fly, which is a lie of omission.
2. **No dots against a live cluster, rail only.** Honest, but it throws away the board's whole point: seeing calls route through kdns, Envoy, and the endpoint pick.
3. **Two flows keyed to the client mode, with an override** (chosen). Both flows share the trace, the anchors, and the path, so nothing the traced flow teaches is untrue of the live one. Only the clock changes.

## Consequences

- Live flow paces at ~360ms per movement with ~200ms micro-holds at RBAC and EDS, drops the settle dwells, and caps concurrency at 24 flights (beyond that, calls stay rail-only). Deny flares and landing pulses fire in both flows.
- The trace store gets exactly one writer per flow: the dot layer drives it step by step in traced flow, and the panel itself holds the latest finished call (refreshed at most every 1.2s) in live flow.
- The header pill now reads `live` or `mock`, and the sim-speed controls stay chaos-gated, so nothing in the chrome suggests a live cluster can be paused.
- `GET /api/traffic` (ADR 0024) is still unserved, so live flow shows no dots against today's facade. The flow machinery is ready for the feed rather than blocked on it, and the `window.__kliteFlow` test hook previews the behavior on the mock.
