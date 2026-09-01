# The frontend ships before the backend, behind a client seam

The web UI lives in `frontend/`, with source under `frontend/app/`, because a repo that also holds a Go backend shouldn't have a directory called `src`. It runs today against a full in-browser simulator. Every page talks to one `KliteClient` interface. `createMockClient()` wraps a headless TypeScript mini control plane (`frontend/app/sim/`) honoring ADRs 0009–0012, and `HttpClient` is a wired-but-dormant implementation of the ADR 0015 facade over the real ClusterService RPCs. Swapping to klited is a factory change driven by `VITE_KLITE_MODE`.

## Considered Options

1. **Wait for the backend.** No UI work starts until klited serves the facade, which serializes two tracks that share no code and keeps the demo invisible the whole time.
2. **Canned fixtures.** Static JSON plus a few timers gets pixels up quickly, but scheduling, drain choreography, and policy semantics stay undemoable, and the UI gets built against data shapes nothing enforces.
3. **A full in-browser simulator** (chosen). A mini control plane driven by one `advance(dt)` clock does spread scheduling, surge-first drain, agent-owned restarts, istio-lite policy evaluation at call time, and random traffic generation. The UI demos every ADR today, and the simulator doubles as executable documentation of them.

We folded the tooling choices into this decision. **shadcn/ui** components, copied into `frontend/app/components/ui/` and themed to the whiteboard tokens, won over React Flow (its drag-editor interaction model fights the hand-drawn board) and over hand-rolled chrome. The rest:

- **bun** for install, test, and build, so one fast toolchain covers the frontend.
- **Motion** for card and chip choreography. The traffic dots stay hand-rolled rAF over a layout-anchor registry.
- **Zod** schemas as the YAML wire contract, mirroring `api/proto/klite/v1/objects.proto` through the user-facing conventions of `internal/object/codec.go`.
- **Biome** for lint and format, **bun test** plus **@playwright/test** for the suites.
- We rejected TanStack Query (state is one watch stream replayed into a store, so there's nothing to cache), GSAP (a second animation system), and Storybook (workshop tooling this scope doesn't earn).

## Consequences

- The simulator can drift from the Go implementation. Three things hold it in place:
  1. the policy evaluator is one pure function, with a test asserting the traffic path and `policyCheck` return identical verdicts
  2. the TypeScript types mirror the protos, down to `nodeName`, integer drain seconds, `status.unschedulable` for cordon, and `ApplyResult.action`
  3. the SSE wire contract lives as the header comment in `httpClient.ts` for the Go implementer to satisfy
- Mutations use the same channels the CLI does: YAML through `apply()`, plus the real `Scale` and `Drain` RPC shapes as first-class client methods. Mock-only affordances (kill agent, sim speed) hang off an optional `chaos` field the UI renders conditionally, so no simulator-only control can leak into a page built for the real backend.
- Cordon has no ClusterService RPC yet. The mock supports it (the scheduler respects `status.unschedulable`), and `HttpClient.cordon` throws until the RPC lands, which keeps the gap loud instead of silent.
- A store-level regression suite exists because cluster-state assertions alone can lie. The node-removal bug, a MODIFIED emitted after DELETED that resurrected the node in every watch consumer, slipped past the cluster map and only the browser caught it.
- The simulator reconciles `spec.replicas` and nothing else. A template change (say, a new image) lands in etcd and no instance restarts: `templateHash` stays unwritten because rollout belongs to the real backend, and showing the drift honestly beats faking a rollout no ADR has designed.
- `make ui` runs bun, and `bun.lock` is committed.
