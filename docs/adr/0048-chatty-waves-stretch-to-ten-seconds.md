# Chatty waves stretch to ten seconds

ADR 0039's demo apps fire in synchronized 8-second waves, and the open-internet test broke the margin that period assumed. A call relayed through the tailnet was measured at about 6.5 seconds end to end, so its tail was still drawing on the board when the next wave spawned, and overlapping flights read as one smear instead of discrete calls. The user chose to stretch the wave to 10 seconds and scale the die from 13107 to 16384 (20% to 25% per wave), which keeps the effective rate at the 2.5% per second that ADR 0039 fixed. The frontend generator changed first (frontend/app/lib/yamlgen.ts, with running workloads rolled onto it), and the seed apps follow in this commit, preserving the byte-parity rule from 0039: the seeds and the dialog generator must produce the same script.

## Considered Options

1. **Keep 8-second waves and accept overlap.** Free, but the board's discrete-call story is the demo's spine, and a smear over the one flight the audience most wants to watch (the remote hop) is the wrong place to save.
2. **Stretch to 10 seconds with the die at 25% (chosen).** The slowest observed flight lands with seconds to spare, and the average call rate every gate and every doc quotes stays 2.5% per second.
3. **Lower the roll rate instead.** Fewer calls per wave reduces the chance of overlap but doesn't bound it, since any wave whose roll hits still races the same 8-second boundary.

## Consequences

- Average traffic is unchanged, so demo.sh's cumulative chatter checks and the traffic feed's expectations hold without retuning. Only boundary spacing moves: calls cluster every 10 seconds now.
- The wave parameters live in three places by design (yamlgen.ts, the four seed apps, demo.sh's narration) and must move together. This ADR is the record that they did.
- Instances created before the retune keep 8-second waves until their workload rolls. The live cluster was rolled by the frontend session; anyone reapplying old YAML gets the old cadence and a parity break with dialog-created services.
