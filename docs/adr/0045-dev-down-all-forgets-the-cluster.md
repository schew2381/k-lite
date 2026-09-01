# dev-down --all forgets the cluster

A k-lite cluster is nothing but its state: records in etcd, the CA and admin token in `~/.klite/server`, node identities in `~/.klite/agent`. Teardown was leaving all three behind. The etcd containers bind-mount `~/.klite/etcd`, so even `dev-down.sh --all` — the nuclear option — removed the containers and kept their data, and the next dev-up silently resumed the old cluster: three-hour-old records on a one-minute etcd, stale node indexes of the exact kind ADR 0042 exists to survive, and join tokens that outlived the "fresh" boot they predated. demo.sh knew, and carried its own private rm block as a workaround. The decision: `--all` now removes the cluster itself. `etcd-up.sh down` grows a `--wipe` flag that deletes its prefix's data directories, and dev-down `--all` invokes it, removes `~/.klite/server`, and removes this profile's node identity directories. A plain `dev-down.sh` still keeps every byte, so stopping and restarting a playground stays cheap.

## Considered Options

1. **Keep the leak, document the manual rm.** demo.sh already proved where this leads: every script that needs freshness grows a private copy of the wipe, and the one path everyone actually uses (dev-up after dev-down --all) stays wrong.
2. **Wipe on every dev-up instead.** Fixes freshness at the entry point, but kills the legitimate resume: a plain dev-down / dev-up cycle around a code change would cost the whole cluster every time.
3. **`--all` wipes, plain down keeps (chosen).** The two teardown levels get honest meanings: plain down stops processes and containers, `--all` ends the cluster.
4. **Unmount etcd's data entirely.** No bind mount, no leak — and no surviving a plain down either, which demotes etcd from the durable store the architecture claims (ADR 0005) to a per-boot cache.

## Consequences

- `--all` invalidates every issued credential: node certificates, join tokens, the admin token. A real machine that joined the old cluster re-joins the new one with a fresh token, which join.sh already handles.
- The wipe is scoped, not global: etcd directories by `ETCD_NAME_PREFIX`, identities by this profile's node names. Side-by-side clusters under other prefixes keep their state, and verify-harness leftovers die with their own scripts.
- demo.sh's private wipe block is now redundant. It stays as a second lock on the same door — the demo is the one run that can't afford a stale CA.
- `AGE` in `klite get` output finally tells the truth after a reset. Its lie (hours-old records on a fresh boot) is what surfaced the leak.
