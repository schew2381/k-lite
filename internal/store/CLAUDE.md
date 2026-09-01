# internal/store

The Store interface and its etcd implementation — the only path to cluster state (ADR 0005). klited stays stateless because everything durable lives behind this package.

Invariants:

- Keys are `/klite/v1/<lowercase-plural-kind>/<name>`, values are protojson.
- `Put` semantics ride the expectedRev sentinels: RevCreate (create-only), RevAny (blind upsert), positive (CAS on mod revision). Callers retry conflicts, the store never loops for them.
- `resource_version` is filled from etcd's ModRevision on every read and never persisted.
- Watch maps etcd events to ADDED/MODIFIED/DELETED using PrevKV. Consumers must tolerate compaction by re-listing and resuming — the stream surfaces the error, it does not hide it.
