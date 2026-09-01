# The facade starts local agents for one-click joins

Joining a node is two different stories. A machine across the internet needs a human to run `klite-agent` there, so the UI hands over the command. On the cluster machine that same command was busywork: the dialog told you to open a terminal on the box you were already looking at. The join dialog's "this machine" path is now a button. It posts `POST /api/nodes/{name}/join`, and the facade mints a token and starts the agent itself. The spawn mirrors `hack/dev-up.sh` down to the flags and the log and pidfile layout under the dev directory, and the agent gets its own session so it outlives a facade restart.

## Considered Options

1. **Keep the copied command for both homes.** No new surface, but the local path stalls the demo for a terminal round trip that adds nothing. The user asked for the button.
2. **Teach klited to start agents.** klited is the wrong body for this. It manages declared state and can't reach machines that haven't joined, so the route would work for exactly one machine while implying it works for all. It would also weld the server to a dev-only process layout.
3. **The facade spawns `klite-agent`** (chosen). It already runs on the machine klited runs on, already holds the admin credentials that mint join tokens, and is already the dev-tool layer where a process-spawning convenience belongs.

## Consequences

- The route arms only when `-agent-bin` resolves (default `bin/klite-agent`) and answers 501 otherwise, so a facade pointed at a remote cluster degrades to the copied command. A pidfile check refuses to start a second agent for the same node.
- The facade now spawns processes on HTTP request. It listens on loopback by default and carries the same trust model as every other route: whoever reaches the facade already holds admin power over the cluster, since the facade attaches its bearer token to each RPC.
- Button-joined agents are indistinguishable from dev-up's. Logs and pidfiles share `KLITE_DEV_DIR`, so `hack/dev-down.sh`'s pidfile sweep reaps them too.
- Internet joins stay a copied command, now built on the browser's hostname instead of the loopback address the dialog used to print. No facade route can reach a machine it doesn't run on.
