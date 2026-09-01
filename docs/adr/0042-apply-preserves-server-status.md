# Apply can't write status, and heartbeats restore a lost node index

Incident 113013 started with an operator re-applying node YAMLs during delete-and-rejoin churn. Two drains had already emptied their nodes, so the controller had removed the records (ADR 0033's choreography). The re-applied YAMLs recreated them with no status while both agents kept running infra on the indexes Register had given them. To `freeNodeIndex` those indexes now looked free, so a rejoining node was handed index 1. Its donor and the incumbent's fought over 10.44.0.11 in a two-second evict-recreate loop until DNS flapped cluster-wide. The update path already carried stored status forward on re-apply, but nothing said so and nothing tested it. A record that lost its status anyway stayed READY-with-no-index forever, because assignment only happened at Register.

This record pins the ownership convention and adds the repair. Spec and labels belong to whoever writes YAML, while meta identity and status belong to Register, ReportStatus, and the controllers. Apply drops a client-sent status block and says so in the action string ("updated (client status ignored)") rather than silently. The YAML that triggers this is usually a re-applied `klite get` export, and its sender should learn what the server kept. And every heartbeat now carries the index the agent's infra actually runs, so the server restores a stored index that came back unset.

## Considered Options

1. **Refuse to recreate a node whose agent might still be live.** The server can't tell a genuinely new node from a resurrected one, and blocking the re-apply would also block ADR 0033's cancel-a-pending-delete path, which is load-bearing.
2. **Let the node controller heal READY nodes with no index.** The controller can see the damage but not the truth. Only the agent knows which index its infra runs, and a guessed re-assignment recreates the exact collision the incident had.
3. **Heal at Register.** Register already assigns when the stored index is unset, but it only runs when an agent restarts. The damaged window is precisely while the agent keeps running, and a restart-time assignment may hand back a different index than the donor still holds.
4. **The agent reports its bootstrap index on every heartbeat, and the server restores an unset stored index from it** (chosen). The fix is one additive proto field, and the heal is restore-only. It never overwrites a set index, never takes an index another record already holds, and yields on a register collision the same way Register does. The damage repairs itself within one five-second heartbeat.

## Consequences

- Apply's merge convention is now contract, pinned by tests: incoming spec and labels win, stored status rides through wholesale, and the no-op check still fires on identical re-applies. CAS semantics are untouched.
- Anything matching apply action strings must treat them as prefixes, since "created", "updated", and "unchanged" can grow the "(client status ignored)" note.
- A window survives the heal. Between the statusless re-create and the next heartbeat, Register can still hand the live index to a joiner. The heal then refuses to steal it back and logs both sides loudly, so the fight never starts. The losing node keeps no index until its agent restarts and re-registers.
- Agents that predate the field report index zero, which the server reads as "nothing reported". Old agents simply don't heal.
- verify-m7 gained the churn step: node YAMLs re-applied mid-run, indexes asserted before and after, and a 30-second watch of agent logs for the fight's two signatures. Tree-built m7 stacks now pass the per-cluster infra knobs (ADR 0030), so the harness runs beside the canonical dev cluster instead of colliding with it.
- ADR 0033 stands as written. Labels stay client-owned on purpose, and re-apply still cancels a pending delete. This record only moves status out of the client's reach.
