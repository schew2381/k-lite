# gRPC-Go research for M1 proto/service design (feeds ADR 0004)

Sources: grpc-go checkout at `~/code/grpc-go` (files cited by path), grpc.io guides, protobuf.dev, buf.build docs.

## Stream design for AgentService

**WatchDesired (server-streaming).**
`rpc WatchDesired(WatchDesiredRequest) returns (stream DesiredState)`. The request carries `node_name` and `last_applied_version`. The server sends a full snapshot when the version is stale or unknown, then deltas or fresh snapshots as state changes. The agent sends nothing after subscribing, so bidi would buy only an ack path. Acks belong in status reporting, where they carry observed state. A broken stream surfaces as a `Recv` error on the agent, which re-dials and re-watches with its last version. This is the Kubernetes watch shape.

**ReportStatus (unary, every 5s), not client-streaming.**
Client-streaming looks natural for a periodic reporter but fails the failure-detection test. `SendMsg` "does not wait until the message is received by the server. An untimely stream closure may result in lost messages" (`~/code/grpc-go/stream.go:116-130`). On error the client gets `io.EOF`, and the real status only appears via `RecvMsg`. A stream reporter can pump into a dead connection until keepalive fires, and the single trailing response of a client-stream can't ack per report. Unary gives each report its own 3s deadline, so a dead klited is detected within one tick. Each call also re-picks a backend under `round_robin`, since streams "cannot be load balanced once they have started" (grpc.io/docs/guides/performance/). Status is idempotent, so a lost report is repaired by the next tick. One HEADERS frame per 5s on an existing HTTP/2 connection costs nothing worth optimizing. If we ever need per-event streaming with acks, that's bidi, not client-streaming.

**Command channel (paired unidirectional streams).**
- `rpc StreamCommands(StreamCommandsRequest) returns (stream Command)`, long-lived, one per agent.
- `rpc PushCommandOutput(stream CommandOutput) returns (CommandOutputSummary)`, one per executing command. The first message carries `command_id`, the final message carries exit status, and the server acks in the summary.

Why paired unidirectional instead of one bidi channel:

- Each command output stream gets its own context, so cancelling one command kills its log flow without touching the command channel.
- HTTP/2 per-stream flow control isolates a log firehose from command delivery. A single bidi stream shares one flow-control window, where a slow reader head-of-line-blocks everything.
- The protos stay flat, with no `oneof` mux plus demux state machines on both ends.

Cost is more concurrent streams per agent, bounded by commands in flight. Cap with `grpc.MaxConcurrentStreams` (`~/code/grpc-go/server.go:472`) if needed.

**Detecting broken streams fast.** Idle watch/command streams generate no traffic, so dead-peer detection is keepalive's job on both sides:
- Agent: `keepalive.ClientParameters{Time: 15s, Timeout: 5s, PermitWithoutStream: true}` via `grpc.WithKeepaliveParams` (pattern: `~/code/grpc-go/examples/features/keepalive/client/main.go:38-42`). Values below 10s are clamped to 10s (`~/code/grpc-go/keepalive/keepalive.go:36`).
- klited: `keepalive.EnforcementPolicy{MinTime: 10s, PermitWithoutStream: true}`, because the default `MinTime` is 5 minutes and a client pinging faster gets a GOAWAY with ENHANCE_YOUR_CALM (`~/code/grpc-go/keepalive/keepalive.go:38-43,94-98`, `Documentation/keepalive.md`). Add `keepalive.ServerParameters{Time: 20s, Timeout: 10s}` so klited also notices vanished agents and frees watch goroutines when `stream.Context().Done()` fires (server example: `examples/features/keepalive/server/main.go:39-50`).
- Worst-case transport detection is `Time + Timeout` (~20-30s). The 5s ReportStatus tick is the app-level liveness signal on top: mark a node NotReady after N missed reports.
- Everything on the agent runs under one root context. Cancel it and all streams end (`examples/features/cancellation/`).

## mTLS + dual auth

**Server requiring client certs** (`~/code/grpc-go/examples/features/encryption/mTLS/server/main.go:67-73`):

```go
tlsConfig := &tls.Config{
    ClientAuth:   tls.RequireAndVerifyClientCert, // see dual-auth note below
    Certificates: []tls.Certificate{serverCert},
    ClientCAs:    caPool,
}
s := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), ...)
```

Agent side mirrors it with `Certificates` + `RootCAs` + `ServerName` (`mTLS/client/main.go:58-63`).

**Extracting identity in an interceptor.** `peer.FromContext(ctx)` (`~/code/grpc-go/peer/peer.go:80`) returns the peer, and `p.AuthInfo.(credentials.TLSInfo)` exposes `State tls.ConnectionState` plus an experimental `SPIFFEID` field (`~/code/grpc-go/credentials/tls.go:41-46`). Node identity comes from the `State.VerifiedChains[0][0]` subject CN or URI SAN. For streaming RPCs the interceptor reads `ss.Context()` (`examples/features/interceptor/server/main.go`, `streamInterceptor`). Stash the resolved principal in the context via the `wrappedStream` pattern from the same file.

**Two auth modes, one server.** Run one listener with `ClientAuth: tls.VerifyClientCertIfGiven` and one shared auth interceptor. If `VerifiedChains` is non-empty the caller is `node:<name>`. Otherwise require a bearer token in the `authorization` metadata key (keys are lowercased, `examples/features/authentication/server/main.go:100-112`), yielding `user:<name>`. Then enforce per service. AgentService accepts only node principals, the CLI-facing service accepts only user principals, and everything else is denied by default. The CLI attaches its token with `grpc.WithPerRPCCredentials` (`oauth.TokenSource`), which refuses to send over a non-TLS transport (`Documentation/grpc-auth-support.md`). The two-listener alternative (two `grpc.Server` instances sharing one service implementation, since `grpc.Creds` is per-server) buys a firewallable agent port but doubles the endpoint config agents and CLI must carry across 2-3 klited hosts. Not worth it at this scale: the single-listener risk is authz sloppiness, which the deny-by-default interceptor addresses.

## Failover & keepalive settings

- Construct clients with `grpc.NewClient`. `grpc.Dial`, `WithBlock`, and `DialContext` are deprecated (`~/code/grpc-go/Documentation/anti-patterns.md`).
- Static 2-3 endpoint list: register a `resolver/manual` builder seeded with `resolver.State{Addresses: [...]}` and dial `klite:///control-plane` with `grpc.WithDefaultServiceConfig("{\"loadBalancingConfig\": [{\"round_robin\":{}}]}")`, the exact pattern in `examples/features/load_balancing/client/main.go:76-80,93-127`. `grpc.WithResolvers` is still marked experimental (`~/code/grpc-go/dialoptions.go:769-775`), so register globally with `resolver.Register` at startup, as the example does. A `dns:///` target with multiple A records also works and is `NewClient`'s default scheme.
- Reconnect backoff: `grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: 1s, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 10s}, MinConnectTimeout: 5s})`. The stock `backoff.DefaultConfig` tops out at `MaxDelay: 120s` (`~/code/grpc-go/backoff/backoff.go`), a two-minute blind spot for an agent, so cap it.
- CLI calls: `grpc.WaitForReady(true)` plus a context deadline. Default behavior fails RPCs immediately in TRANSIENT_FAILURE, while wait-for-ready queues them until a backend connects or the deadline hits (`~/code/grpc-go/rpc_util.go:334-341`, `examples/features/wait_for_ready/main.go:93`, `anti-patterns.md`).
- Server death mid-stream: gRPC never resumes a stream. `Recv` returns `codes.Unavailable`, the `ClientConn` redials per backoff, and `round_robin` steers the next RPC to a live backend. Re-invoking `WatchDesired`/`StreamCommands`, though, is application code. Service-config `retryPolicy` (`examples/features/retry/`, gRFC A6) replays only RPCs that received no message, so it can't rescue an active watch. The agent's reconnect loop with `last_applied_version` resume is mandatory, not an optimization.
- `round_robin` spreads unary calls, but long-lived streams pin to whichever backend accepted them. If agents clump on one klited after a failover, `keepalive.ServerParameters.MaxConnectionAge` plus `MaxConnectionAgeGrace` (`examples/features/keepalive/server/main.go:44-50`) forces periodic graceful re-spread. That's safe once version-resume works, since a GOAWAY just triggers the normal reconnect path.

## Toolchain recommendation

Use buf, pinned in `go.mod` alongside both codegen plugins via the Go 1.24+ `tool` directive. Generation then runs offline from the module cache and `go.sum` pins every version. Bare protoc can't give reproducible builds: it's a separately installed C++ binary whose version lives outside the repo, and grpc-go's own Makefile just probes `which protoc` (`~/code/grpc-go/Makefile:12-14`). buf bundles its compiler and runs local plugins without network access (buf.build/docs/configuration/v2/buf-gen-yaml/).

```
go get -tool github.com/bufbuild/buf/cmd/buf@v1.55.1            # pin current
go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

`buf.yaml` (repo root): `version: v2`, `modules: [{path: api/proto}]`, `lint: {use: [STANDARD]}`, `breaking: {use: [FILE]}`.

`buf.gen.yaml`:

```yaml
version: v2
plugins:
  - local: ["go", "tool", "protoc-gen-go"]
    out: internal/gen
    opt: paths=source_relative
  - local: ["go", "tool", "protoc-gen-go-grpc"]
    out: internal/gen
    opt: paths=source_relative
```

Makefile target: `proto: ; go tool buf generate && go tool buf lint`. CI adds `go tool buf breaking --against '.git#branch=main'` and `git diff --exit-code internal/gen` to catch uncommitted regeneration. Sources live in `api/proto/klite/v1/{agent,cluster,common}.proto` with `package klite.v1;` and `option go_package = "github.com/<owner>/k-lite/internal/gen/klite/v1;klitev1";`. Generated code is committed under `internal/gen/klite/v1/`, so builds never invoke codegen. Fallback if we drop buf: the quickstart command `protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative <files>` (grpc.io/docs/languages/go/quickstart/) with the same pinned plugins.

## Versioning / compat rules to bake in

Per protobuf.dev/best-practices/dos-donts/:

- Never reuse a field number.
- Add `reserved` for both numbers and names when deleting fields or enum values.
- Never change a field's type or convert repeated to scalar.
- Start every enum with `<NAME>_UNSPECIFIED = 0`.

proto3 has no `required`. Keep it that way by validating presence in handlers, not by treating absence as fatal in decoding. Breaking changes mean a new `klite.v2` package alongside `klite.v1`, never edits in place. `buf breaking` in CI enforces the whole list mechanically.

## Pitfalls

1. Keepalive mismatch kills connections. An agent pinging faster than the server's `EnforcementPolicy.MinTime` (default 5 minutes) gets a GOAWAY ENHANCE_YOUR_CALM, and the client's ping interval silently doubles afterward (`keepalive/keepalive.go:38-43`). Setting client keepalive without the matching server policy is the classic failure.
2. Client `Time` below 10s is clamped to 10s (`keepalive/keepalive.go:36`). Don't design for 5s transport detection. That job belongs to ReportStatus.
3. Stream sends lie. `SendMsg` succeeds into a broken stream and hides the status behind `RecvMsg` (`stream.go:116-130`). Any long-lived send loop needs a concurrent `Recv` to observe death.
4. Streams neither fail over nor replay, and no retry policy applies once a message has arrived (gRFC A6). Budget for the reconnect-and-resume loop in the agent from day one.
5. Both sides cap received messages at 4MB by default (`server.go:61`, `clientconn.go:139`). Chunk command output (~64KB messages) and keep desired-state snapshots under the cap instead of raising limits.
6. `NewClient` is lazy, so nothing connects until the first RPC. Gate agent startup on the first successful `ReportStatus`, not on "connected" (`anti-patterns.md`).

## Verdict for ADR 0004

The decision holds. All three agent flows map onto stock gRPC shapes (one server-stream, one unary, one paired stream set) multiplexed over a single mTLS connection per agent, with the CLI reusing the same failover machinery. Nothing here needs a sidecar protocol or custom framing. The costs are known and bounded. Carry these four into M1 up front:

1. Keepalive config on both sides.
2. A principal-resolving auth interceptor.
3. A reconnect-with-resume loop in the agent.
4. Committed generated code.
