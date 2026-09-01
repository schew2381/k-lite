# The UI gains an etcd browser and step-traced requests, reopening ADR 0015's page freeze

Watching the demo produced two asks the four fixed pages couldn't answer: show what etcd actually holds, and slow a single request down until every hop is legible. The UI now has a fifth page, `#/etcd`, that lists every key with its mod revision, flashes rows as revisions move, and expands any row to the full object as YAML. On the topology page, one call at a time plays as a traced flight, and the dot moves exactly when packets do:

1. holds on the caller for the in-process resolve
2. rides out to the infra pod's kdns with the DNS query
3. rides back to the caller carrying the VIP answer
4. rides out again as the dial, landing on Envoy's listener
5. holds at the RBAC filter for the verdict
6. holds at EDS for the endpoint pick, then crosses to the picked instance

We slowed traffic to one call per 30 seconds so a story finishes before the next begins.

## Considered Options

1. **Keep the four-page freeze and stuff the detail into the inspector sheet.** The infra-pod inspector already shows kdns, LDS, RBAC, EDS, and the identity map per node, but it answers "what is programmed", not "what does etcd hold" or "what happened to this request".
2. **A raw JSON dump behind a debug flag.** Cheap, unreadable, and it teaches nothing about revisions or watch behavior.
3. **A real etcd browser page plus staged request traces** (chosen). Both render straight from the existing watch-driven store, so they cost no new client surface: the browser is the snapshot keyed etcd-style, and the trace is built from each TrafficEvent plus the snapshot at spawn.

## Consequences

- ADR 0015's "exactly four pages" is superseded on the page count. The rest of it stands, including the facade endpoint list as amended by ADR 0024.
- The simulator stamps `metadata.resourceVersion` on every emit, matching etcd's mod-revision semantics, and the real facade must do the same (it already gets this from etcd for free).
- Traffic pacing became a UI concern: the demo runs one traced call per beat, and tests inject dense timings instead of inheriting demo pace.
- We checked the trace steps against the records they narrate:
  1. search-domain expansion and the single upstream (ADR 0008, ADR 0017)
  2. the per-node VIP answer with TTL 5s (ADR 0006, ADR 0017)
  3. the LDS listener bound on the VIP (ADR 0007, ADR 0008)
  4. RBAC with the xDS identity map, DENY-first and ALLOW-flip (ADR 0009)
  5. READY-only round-robin with DRAINING excluded (ADR 0010)
- A first cut parked the dot on the caller through the whole DNS exchange and compressed the path into two hops, which read as if the node answered by rewriting the query in place. That is kube-proxy's trick, not ours: k-lite has no DNAT layer. The query is a real packet to kdns (ADR 0008 draws the arrow), the answer is a real packet back, and the dial that follows is a second, separate connection to a VIP that Envoy has bound via `freebind` (ADR 0007). The animation now plays all four movements so the mechanism the ADRs chose (addressing, not interception) is the one on screen.
