# Go, one module, four binaries

k-lite is written in Go (1.26 minimum, exact toolchain pinned in go.mod) as one module, `github.com/schew2381/k-lite`, that builds four binaries:

1. `klited`, the control plane.
2. `klite`, the CLI.
3. `klite-agent`, one per node.
4. `klite-net`, the per-node DNS and VIP companion.

Envoy and etcd arrive as upstream container images, not as code we build.

## Considered Options

1. **Go** (chosen). The ecosystem we're imitating is Go end to end, from k3s and swarmkit to nomad, go-control-plane, and etcd's client. The Docker SDK is first-party, and static cross-compilation makes the scratch-image companion a one-liner.
2. **TypeScript on Node.js.** It's already on this machine and shares a language with the UI, but long-running daemons and single-binary distribution fight the platform.
3. **Python.** Fastest to sketch and worst to ship, since we'd be distributing three cooperating daemons.
4. **Rust.** There's nothing wrong with it except iteration speed on a project this wide.

## Consequences

- The UI keeps its own Node.js toolchain. Its build output embeds into `klited` via go:embed, so at runtime the answer stays "run one binary."
- `klite-net` cross-compiles with CGO off (GOOS=linux, GOARCH=arm64) into a scratch image.
- All code follows the Go practices already pinned in CLAUDE.md, from gofmt to table-driven tests under `-race`.
