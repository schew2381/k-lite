# The UI is four pages and a policy simulator

The UI is React + Vite + Tailwind, built with the frontend-design skill. It compiles to static files, embeds into `klited` via go:embed, and stays live through the REST/SSE facade. It ships four pages, plus a "can A reach B?" simulator backed by the PolicyCheck RPC:

1. Cluster topology, showing nodes and their instances, services with VIPs, and policy edges in red and green.
2. Resource tables.
3. Live logs.
4. A YAML apply box.

## Considered Options

1. **Topology and apply only.** Enough for debugging, but the policy model would have no surface of its own.
2. **The four pages, nothing else.** This was the plan before the simulator earned its slot. The policy evaluator is a pure function, so exposing it costs one endpoint and one form.
3. **Four pages plus the simulator** (chosen).

## Consequences

- This ADR freezes the facade's route list, so a UI wish that needs a new route reopens the decision rather than growing quietly.
- There are no auth screens and no editing views beyond the YAML box. The CLI remains the primary interface, and the UI's job is to make the cluster legible.
