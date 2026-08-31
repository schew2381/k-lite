# gRPC everywhere, and agents always dial out

Agents, the CLI, and each node's Envoy all speak gRPC to `klited`, and the protos under `api/proto/klite/v1` are the source of truth. Connections only ever go from agent to server (streams carry desired state down and status up), so a node behind NAT joins a WAN cluster with no design change. The browser is the one exception and gets a small hand-written REST and SSE facade.

```
klite CLI ──gRPC──▶ ┌────────┐ ◀──gRPC──── klite-agent   (dials out, per node)
browser ──REST/SSE▶ │ klited │ ◀──xDS/gRPC─ envoy        (dials out, per node)
                    └────────┘
```

## Considered Options

1. **HTTP long-poll plus SSE plus callback POSTs.** This was the original recommendation, curl-debuggable and toolchain-free. The durability requirement killed it, because we want typed contracts and the same wire idioms as the systems we imitate, and xDS itself is gRPC.
2. **gRPC** (chosen). It brings typed streams, mTLS in the transport, client-side failover across servers, and an xDS server that shares the listener.
3. **A WebSocket protocol**, k3s remotedialer-style. It needs only one connection, but we'd be hand-designing a framing protocol that gRPC already is.
4. **grpc-gateway or gRPC-web for the browser.** They add codegen or a proxy layer to serve five routes, when writing the facade by hand is smaller.

## Consequences

- A proto toolchain enters the build (`make proto`).
- The facade can drift from the protos. If it grows past a handful of routes, option 4 gets revisited.
- The server can never call an agent. Log streaming, and any future exec, rides streams the agent opened.
