# The frontend adopts the facade's real dialect

klite-facade now exists (`internal/facade`), and it differs from the wire contract the UI guessed at in ADR 0023:

- routes live under `/api`
- lists arrive as `{"items":[...]}` in the codec's user-facing JSON, while watch events are raw protojson
- the Watch RPC delivers changes only, with no resume
- logs stream as chunked text rather than SSE
- policycheck answers an availability envelope
- per-node VIPs live in the server-materialized `VIPAllocation` kind (ADR 0022) instead of a Service enrichment

The frontend now speaks all of that. A decoder normalizes both wire dialects into one canonical shape, the client bootstraps with list-then-watch and resets on reconnect, and the UI reads every VIP it shows from a `VIPAllocation` object in both mock and http modes.

## Considered Options

1. **Change the facade to match the UI's guessed contract.** The facade is uncommitted and possibly mid-edit in another session, and its choices are the defensible ones (protojson passthrough, no bespoke resume machinery). Rewriting someone's in-flight work to satisfy a guess inverts the burden.
2. **Keep two type layers, one per dialect.** Every consumer would need to know where an object came from, which is the exact coupling the client seam exists to prevent.
3. **One decoder at the seam, canonical types everywhere else** (chosen). `decode.ts` accepts either dialect (codec JSON or protojson) and everything past the seam sees only the canonical shape. The mock now materializes `VIPAllocation` objects with the same reconcile-and-release semantics as ADR 0022, so the etcd browser and the kdns tables read identically against either backend.

## Consequences

- The watch has no replay and no resume, so `HttpClient.watch` lists every kind, synthesizes ADDED events plus SYNC, follows the SSE stream, and on any reconnect emits RESET and bootstraps again. Between list and watch there's a window where the client misses any change until the next event arrives. Closing it needs the facade to expose `Watch.from_revision`, recorded here as the known gap.
- Scale rides apply (fetch the Workload, bump `replicas`, POST the document) because the Scale RPC has no facade route yet. Drain has neither a route nor a YAML path, so `drainNode` fails loudly in http mode while "Drain & remove" works through DELETE. `GET /api/traffic` (ADR 0024) is still unserved, and the rail's waiting state is the degradation.
- `policyCheck` returns `{available:false}` until klited implements the RPC, and the simulator panel says so instead of erroring. When available, the reason is the evaluator's own sentence. The mock words its verdicts identically to `internal/policy`, so the panel reads the same against either backend.
- The client rejects an applied `Instance` or `VIPAllocation` with the server's rationale: both kinds are server-materialized.
- Delete a `VIPAllocation` by hand and the mock's next reconcile sweep recreates it, matching the leader controller's level-based behavior. The drain suite asserts allocations release when their node leaves.
