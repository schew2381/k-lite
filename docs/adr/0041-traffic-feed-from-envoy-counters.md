# The traffic feed streams Envoy counter deltas

ADR 0024 reopened the facade for `GET /api/traffic` and sketched it over Envoy access logs, then deferred it until a real backend existed. The backend now exists and the seeded apps chatter through it (ADR 0039), yet live mode showed an empty board because nothing served the feed. It's served now, from a different source than the sketch. The facade polls each Ready node's Envoy admin once a second while anyone is subscribed, reads `/clusters` request counters per upstream host, and streams the deltas as SSE. Each delta names the caller's node, the target service, and the exact dial target. The frontend resolves that target against its snapshot: an instance IP means a local call, while an ingress port names the instance behind a machine address.

## Considered Options

1. **Envoy access logs, as ADR 0024 sketched.** They have the highest fidelity, including the caller's IP and therefore its instance. But nothing collects access logs today: enabling them lives in the agent's Envoy bootstrap (the backend's territory), and the lines would still need shipping out of each infra container. That's the most moving parts for the same dots.
2. **Tail every instance's runtime logs and parse the chatty `-> b ok` lines.** The caller is known because it wrote the line, but the feed would be welded to one demo app's log format and cost a log stream per instance.
3. **Poll the Envoy admins and stream counter deltas** (chosen). The agent already publishes each admin on loopback at 19500 plus the node index, and `/clusters` counts requests per upstream host. One HTTP poll per node per second, no backend changes, and the poll skips the ingress-side clusters so a remote call counts once.

## Consequences

- The caller's instance is unknown to the counters alone, since one Envoy fronts every instance on its node. The kdns RecentQueries ring closes that gap: every chatty call resolves its target first, kdns records the query's source IP, and the facade splits a delta into single calls that carry their caller. Events the ring can't match stay node-attributed, with the trace starting at kdns instead of a chip, and a donor without the ring degrades to exactly that. The mock keeps instance attribution because the simulator genuinely knows it.
- Counters count and do nothing else, so live events carry no latency. The rail prints it only when it's known. Denials ride the same poll from each listener's RBAC filters, whose per-phase stat prefixes say whether the DENY phase or the allowlist killed the call, and the board flies a red mark that dies at the RBAC box.
- A remote machine's admin is loopback on that machine, so its outbound calls are invisible here. Calls to it still appear, counted by the caller's Envoy. A multi-machine feed needs nodes to export their counters, which is future work.
- The poll baseline lives only while subscribers exist and dies with the last one, so a fresh subscriber never gets history replayed as a burst.
- ADR 0024 still names the route and its place in the facade. This record settles what finally feeds it.
