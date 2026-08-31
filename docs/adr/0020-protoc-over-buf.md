# Codegen runs on protoc, despite the research recommending buf

research/grpc-go.md recommended buf pinned through go.mod's tool directive. We generate with plain protoc anyway. Brew had already installed it and the two plugins during bootstrap, and `make proto` over four proto files doesn't need lint, breaking-change detection, or a second toolchain. The generated code is committed under internal/gen, so builds never regenerate.

## Considered Options

1. **buf via `go tool`** stays reproducible and offline, and brings linting and breaking-change checks. All of those are real advantages, and none of them binds at four files with one author.
2. **protoc + protoc-gen-go + protoc-gen-go-grpc** (chosen) was already on the machine, needs one Makefile target, and adds no new concepts.

## Consequences

- Anyone regenerating needs protoc on PATH (the Makefile prepends `$(go env GOPATH)/bin` for the plugins).
- If the proto surface grows past a handful of files or gains a second consumer, buf's checks start paying rent and this ADR should be superseded.
