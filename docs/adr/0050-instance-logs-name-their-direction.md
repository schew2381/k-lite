# Instance logs name their direction

Tailing a node's instances during the hotspot test, the user couldn't tell traffic they were sending from traffic they were serving, because the chatty script only ever logged its own outbound rolls. Two changes close that. Outbound lines now say so: `send -> b ok` instead of `-> b ok`. And httpd runs verbose in the background (`(httpd -f -vv -p 80 -h /www &)`), so every served request logs a receive pair like `[::ffff:10.44.0.11]:port: url:/` beside the sends. Bare TCP connects log nothing, which was the load-bearing fact checked before committing: the readiness probes that hit :80 every few seconds stay silent, so the receive lines are real requests only. The subshell wrapper keeps the generator's `'; '` join valid, since a bare `&` followed by `;` is a shell syntax error. The `send ->` prefix ripples into every gate that parses chatter (verify-m4 and demo.sh), and the byte-parity rule from ADR 0039 applies as always: the frontend generator mirrors the same script.

## Considered Options

1. **A tail-side wrapper instead of a script change.** Prefixing `docker logs` output can label what exists, but receives don't exist in the logs at all without the httpd change, so no wrapper can surface them.
2. **Verbose httpd plus a send prefix (chosen).** Two tokens of script change, probe-silent, and the two directions read differently at a glance.
3. **Name the denying policy on failed sends.** Asked for, and skipped with the user's blessing. A denied connection is a bare TCP reset from the source Envoy's RBAC filter, indistinguishable in-container from an unreachable endpoint, and carrying no reason. Naming the policy would need an in-cluster "why" oracle or the access-log collection ADR 0041 already declined. The policy verdict lives where the knowledge is: the board's reach checker and `klite policy check`.

## Consequences

- Receive lines name the peer as the node's own Envoy, not the true caller, because every data-path hop terminates there. Caller attribution stays kdns's job (ADR 0044); the receive line proves serving, not identity.
- Old-format instances keep logging `-> b ok` until their workload rolls onto the new template. Gates parse the new format only, so they hold for fresh boots and rolled clusters, not mid-roll snapshots.
- The wave parameters' three-places rule (ADR 0048) now covers the log grammar too: the script, the generator, and the gates that grep it move together.
