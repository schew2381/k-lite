# Demo apps chatter, and verify gates stop reading their logs

The seeded a/b/c became chatty on user request: each instance serves its hostname over httpd and rolls a 5% chance per second to call one random peer service through the real data path, so a resting cluster hums at roughly 17 calls a minute and the board narrates live traffic. That killed the verify scripts' old assertion source (a's predictable request loop), so the zero-failure gates moved to deterministic probers the scripts own: a once-a-second `wget` exec'd inside a's container, riding the same kdns → VIP → Envoy → ingress path, with gates diffing FAILED counts over each churn window and demanding line growth so a dead prober can't fake a clean pass.

## Considered Options

1. **Keep verify fixtures on frozen inline YAMLs and change only the demo.** The premise failed: verify-m3 and verify-m8 also consume examples/apps, demo.sh needed the rework regardless, and the e2e layer would have been testing YAMLs that no longer ship.
2. **Chatty apps everywhere, with gates keeping random chatter as their denominator.** A 5% roll is ambience, not evidence — windows would pass or fail on luck.
3. **Chatty apps everywhere, deterministic probers own the gates** (chosen). The exec'd prober also adds no workload, so every instance-count assertion survived unchanged.

## Consequences

- The chatty script's shape is shared byte-for-byte with the frontend's dialog generator, and landing it surfaced two real bugs upstream: a `'; '` join that produced `do;` (busybox ash exits 2, dialog-created services were dead on arrival) and PID-1 dropping SIGTERM (every stop waited the full 30s grace; 0.07s with the opening `trap "exit 0" TERM`).
- Policy beats got sharper: a deny now gates both the prober hitting 100% FAILED and the chatter logging zero fresh successes to the denied target.
- The Envoy counters the chatter exercises (`cluster.<svc>.upstream_cx_total`, per-port `ssl.handshake`) are the ready-made feed for the UI's future traffic view.
