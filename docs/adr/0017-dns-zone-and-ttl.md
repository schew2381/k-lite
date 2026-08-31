# Names live under svc.klite, answer in 5 seconds, and never deny existing

Service DNS is `<service>.svc.klite`, and instances get `--dns-search svc.klite` so bare names work. Records carry a 5-second TTL. Resolution succeeds even when policy denies the caller, because the connection is what gets refused. "A knows B exists" was a stated requirement, so existence is public and reachability is not.

## Considered Options

1. **NXDOMAIN for denied callers.** It leaks less topology, but it contradicts the requirement and makes a policy denial look exactly like a typo in the service name.
2. **Long TTLs.** It means fewer queries and staler answers, and it's pointless anyway when the VIP behind the name never moves.
3. **Short TTL, always resolve** (chosen).

## Consequences

- A denied call fails fast with a connection reset, cleanly distinguishable from "no such service", which keeps debugging sane.
- The 5s TTL bounds staleness for resolvers that respect it, and the fixed per-node VIP (ADR 0006) covers the ones that don't.
