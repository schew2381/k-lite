# k-lite frontend

This is the live cluster UI. It shows topology with step-traced calls, an etcd browser, resource tables, log tails, and a YAML apply box. It runs against an in-browser simulator (a small TypeScript control plane that schedules, drains, restarts, and evaluates policies the way the ADRs say), so the whole demo works before klited exists.

```sh
bun install
bun run dev        # http://localhost:5173, mock cluster
bun test app       # simulator, store, and layout suites
bun run e2e        # Playwright walkthrough against the dev server
bun run build      # static bundle in dist/, later embedded via go:embed
```

## Layout

```
app/
  api/     KliteClient (the seam), Zod schemas, mock + http implementations
  sim/     the mini control plane: scheduler, policy evaluator, cluster loops
  store/   watch events replayed into an immutable snapshot; traffic ring
  layout/  pure slot math for the board; anchors the dot layer reads per frame
  topo/    node cards, chips, dots, rail, policy builder + simulator,
           control-plane strip, and the infra-pod inspector (kdns, LDS,
           RBAC, EDS, identity map — all derived live from the store)
  pages/   topology · resources · etcd · logs · apply
```

Directory names say which half of the repo you're in: the Go backend owns `cmd/` and `internal/`, the browser owns `frontend/app/`. Nothing is called `src`.

## Swapping in the real backend

Set `VITE_KLITE_MODE=http` and `VITE_KLITE_API` to klited's address. `HttpClient` implements the ADR 0015 facade, and its header comment is the wire contract: the SSE watch shape, the scale and drain routes that map ClusterService RPCs, and the `GET /v1/traffic` route ADR 0024 adds. The mock and the real server see identical requests. Mock-only controls disappear on their own, since the UI renders them only when the client exposes `chaos`.
