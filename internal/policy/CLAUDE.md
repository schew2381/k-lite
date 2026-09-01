# internal/policy

The istio-lite policy evaluator (ADR 0009): DENY always wins, the first ALLOW targeting a Service flips it to allowlist mode, and everything else defaults open. Three consumers share it — the xDS RBAC compiler, the PolicyCheck RPC, and the user's UI simulator.

Invariants:

- Pure functions only. No I/O, no clock, no logging. If a change needs any of those, it belongs in a caller.
- Evaluation order is deterministic regardless of input order (policies sorted by name), so reasons and matched-policy names are stable across replicas.
- `except` lists only mean something when `to` is `"*"`. Validation upstream enforces it, the evaluator assumes it.
