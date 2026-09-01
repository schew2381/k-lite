# internal/object

The one place where YAML becomes typed objects and back: multi-doc codec (yaml → JSON → protojson), validation, defaulting, kind registry, and the template hash.

Invariants:

- Exactly one container per template in v1 (ADR 0014). Validation enforces it, the schema stays a list.
- Defaults live here and nowhere else: drain 30s/15s, maxInstances 32, targetPort falls back to port.
- The template hash drives rollouts. It must stay deterministic (`proto.MarshalOptions{Deterministic: true}` + FNV-1a) — changing its inputs or algorithm re-rolls every Workload in existence.
- The kind registry owns every accepted spelling (singular, plural, lowercase). CLI and API resolve names through it, so new kinds register here first.
