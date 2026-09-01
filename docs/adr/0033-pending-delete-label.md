# Node deletion is a label, and re-applying the YAML cancels it

Deleting a Node doesn't remove its record. The server stamps a `klite.io/pending-delete` label, cordons the node, and marks its instances DRAINING. The node controller removes the record only after the drain empties it. A consequence worth knowing: re-applying the node's YAML overwrites labels and therefore cancels a pending delete.

## Considered Options

1. **A proto status field.** Cleaner typing, but it grows the wire contract for one internal flag during speed mode, and every schema change costs a buf-breaking pass.
2. **Immediate record removal + orphaned drain state.** The drain would need a home outside the object it's about, and a crashed leader would strand it.
3. **A meta label on the record that stays until drain completes** (chosen). The object remains watchable through its own teardown, the drain machinery reads ordinary state, and failover just re-observes it.

## Consequences

- `klite delete node <n>` returns before the record disappears — the leader finishes the choreography.
- Re-apply-cancels-delete is defensible (declaring the node again means you want it) but surprising, hence this record.
- If the flag ever needs to survive label rewrites, promote it to NodeStatus and supersede this.
- M8 added `klite uncordon`, which clears a cordon unless this label is present — a pending delete stays a delete.

## Outcome

Shipped, and the interplay with uncordon settled the way the M8 note above says: `klite uncordon` refuses to clear a cordon that a pending delete owns, so the label is load-bearing in two directions. verify-m5 exercises the delete-by-YAML path (drain first, record removed only when empty) and verify-m8 drains node-wan out through the same choreography. ADR 0037 later pinned the final record removal to the revision the controller read, closing the window where a re-applied node could be deleted by a lagging leader.
