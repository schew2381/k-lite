# The agent earns a face on the node card

The board grew out of the data path, so every box on it was a stop on the DNS, VIP, Envoy, instance story. klite-agent never touches a packet, and it ended up invisible: the strip's stream pills, the Ready badges, and the join dialog all show its effects without naming their cause. The biggest aha in the design, that klited never touches Docker and a per-node agent does the watching and reconciling (ADR 0011), was unlearnable from the screen. Each node card now carries an agent line between its header and its infra pod: alive with its heartbeat age and what it runs, red when it stops beating, muted with a pointer to Join… while the node waits for a machine.

## Considered Options

1. **Leave it implicit.** Free, and wrong for a teaching tool: a newcomer reasonably concludes klited runs the containers itself.
2. **A full sub-box beside the infra pod's.** That's too much weight. Dots never visit the agent, so a box the dot layer serves would imply a data-path role it doesn't have.
3. **A one-line bar fed only by watchable state** (chosen). Node status carries phase and heartbeat, the strip already derived stream health from them, and that arithmetic moved into one shared selector so the pills and the bars can never disagree.

## Consequences

- The bar states are waiting, running, and gone. Mock trusts phase, since the simulator pauses with the page and a frozen heartbeat would lie. Live trusts the heartbeat and shows its age, which keeps aging on a wall-clock tick between watch events.
- The traced story excludes the agent on purpose, per the user's call. Traces narrate a request, and the agent isn't in one. Its one honest trace moment (the scheduler assigned, the agent created) stays unbuilt.
- Node cards grew a fixed 24 pixels. The layout owns that constant, so the dot anchors and the containment tests moved with it.
- A dead agent now explains itself on the card ("nothing reconciles here") instead of only dimming a pill in the strip.
