# A live traffic feed reopens ADR 0015's frozen facade

The topology page animates individual calls: instance to its node's Envoy, then on to the chosen instance, with denials dying at the proxy under the policy's name. That needs per-connection events no frozen facade route carries. The client interface includes `watchTraffic()` now, and the real backend adds one route, `GET /v1/traffic`, an SSE stream of `TrafficEvent` JSON fed from Envoy access logs. ADR 0015 said a UI wish needing a new endpoint reopens the decision. This is that reopening, recorded before the backend exists.

## Considered Options

1. **Keep the facade frozen and fake it.** Against the real backend, dots become derived animation from topology plus policies, which is plausible motion rather than actual connections. The visualization quietly stops being truthful the day real traffic exists.
2. **Poll Envoy admin stats.** No new backend route, but stats are aggregates. Per-call verdicts, the chosen instance, and the matched rule are gone, and those are the whole story.
3. **One SSE traffic route fed from Envoy access logs** (chosen). Envoy already emits an access log per connection with the RBAC outcome. klited tails it, enriches it with the instance identity map, and streams. The mock emits the same `TrafficEvent` shape today.

## Consequences

- The facade's endpoint list grows by exactly one route, and everything else in ADR 0015 stands.
- `HttpClient.watchTraffic()` treats a 404 as "feed not implemented yet" and the UI degrades to the traffic rail placeholder, so the frontend works against a klited that hasn't built this.
- Envoy needs an access-log configuration (gRPC ALS or a tailed file). That backend work is recorded here so it lands with M4 rather than surprising it.
- The event shape lives in `frontend/app/api/types.ts` and carries:
  - id and timestamp
  - caller instance and service, destination service
  - the enforcing node
  - the verdict with its matched rule
  - the landing instance plus latency when allowed
