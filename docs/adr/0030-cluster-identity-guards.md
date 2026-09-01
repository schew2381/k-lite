# Infra containers carry a cluster identity, and eviction respects it

Two clusters sharing one Docker daemon used to be mutually destructive: donor eviction matched squatters by IP alone, so a second cluster's agent would delete the first cluster's infra pod (caught live during M6's isolated verify). Now klited mints a random cluster id once (etcd transaction), hands it out in NetBootstrap, and every infra container carries it as `io.klite.cluster`. Eviction removes only same-cluster or legacy-unlabeled containers — a foreign squatter is a loud error, never a delete. The per-cluster knobs a deliberate second cluster must move (admin port bases, donor IP base) became klited flags carried in the same message.

## Considered Options

1. **Keep IP-match eviction.** Correct in a one-cluster world and destructive in every other one, silently.
2. **Refuse to run two clusters per daemon.** Honest, but the verify harnesses themselves need coexistence, and they're how everything here stays tested.
3. **Identity labels plus explicit knobs** (chosen).

## Consequences

- Legacy unlabeled containers count as our own, which keeps upgrades from stranding old donors.
- Coexisting clusters still share klite0's subnet — the knobs prevent collisions, they don't create isolation. Policies and VIP pools remain per-cluster state in separate etcds.
- The chaos and policy harnesses (m6, m7) can run their own full clusters beside the canonical one at HEAD.
