# Deletes carry the revision they acted on

The controller review found the last CAS hole: every write was revision-checked, but deletes went by bare name, so a lagging leader working from a stale list could delete a doomed instance that had turned READY, an expired drain's recycled namesake, or a node record whose pending delete a re-apply had canceled. `store.Store` gained `DeleteIfRevision` — one etcd transaction on the mod revision, `ErrConflict` when the object moved, `ErrNotFound` when it's gone — and every controller delete path now uses it.

## Considered Options

1. **Keep bare deletes and rely on idempotence.** Recreation converges eventually, but a serving endpoint dying for even a window violates the zero-dip promise ADR 0010 makes.
2. **Redesign Delete itself to require a revision.** Breaks every existing caller for the few imperative paths (user-initiated deletes) where blind intent is the right semantics.
3. **An additive CAS form beside the blind one** (chosen). Level-based loops pin, imperative paths stay blind on purpose.

## Consequences

- The contract lives in the store and controller CLAUDE.mds: NotFound and Conflict both mean "re-observe", never failure.
- Monotonic etcd revisions make "recreated at the same revision" impossible, which is what makes the pin sufficient.
- Server-side candidates (drain force-delete) follow as the security-surface review lands them.
