# Docker SDK: github.com/moby/moby/client, not the frozen docker/docker

The runtime uses `github.com/moby/moby/client` v0.5.x with `api` v1.55, wrapped behind our Runtime interface. The module everyone reaches for, `github.com/docker/docker`, froze at v28.5.2+incompatible in November 2025 and stopped receiving tags.

## Considered Options

1. **github.com/docker/docker** is the path in every tutorial and is now unmaintained as a module. Pinning a frozen +incompatible version invites silent drift from the daemon we actually run (29.x).
2. **github.com/moby/moby/client** (chosen) is actively tagged and netip-typed, but it's v0.x, so minor versions break (`NewClientWithOpts` already gave way to `client.New`). The Runtime interface is the blast shield.

## Consequences

- Exact versions get pinned in go.mod, and upgrades happen deliberately, never via `go get -u`.
- Only `internal/runtime/docker` imports the SDK. Everything else sees our interface, so a breaking v0.x bump stays a one-package fix. research/docker-go-sdk.md has the specifics.
