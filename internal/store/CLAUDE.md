# internal/store

This package holds the Store interface and its etcd implementation, the only path to cluster state (ADR 0005). klited stays stateless because everything durable lives behind this package.

Invariants:

- Keys are `/klite/v1/<lowercase-plural-kind>/<name>`, values are protojson.
- `Put` semantics ride the expectedRev sentinels: RevCreate (create-only), RevAny (blind upsert), positive (CAS on mod revision). Callers retry conflicts, and the store never loops for them.
- `DeleteIfRevision` is Delete's CAS form: positive revision only, ErrConflict when the object moved since the caller read it, ErrNotFound when it's gone. Level-based loops treat both as "re-observe", never as failures.
- `resource_version` is filled from etcd's ModRevision on every read and never persisted.
- Watch maps etcd events to ADDED/MODIFIED/DELETED using PrevKV. Consumers must tolerate compaction by re-listing and resuming — the stream surfaces the error, it does not hide it.
