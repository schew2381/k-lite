# Proto tooling moves to buf, superseding ADR 0020

ADR 0020 kept plain protoc and named its own supersede trigger: the moment the proto surface grows or gains more hands. Both happened at once — subagents now edit the schema across milestones, and the user asked for generation driven by config rather than hand-run commands. `make proto` now formats and generates through buf, pinned in go.mod via the `tool` directive, and `buf lint` plus `buf breaking --against main` guard the schema in prek and CI.

## Considered Options

1. **Stay on protoc.** It kept working, but nothing checks schema style or wire compatibility, and with multiple agents editing protos an accidental field renumber would ship silently.
2. **buf, pinned as a go.mod tool** (chosen). One `buf.gen.yaml` drives generation, `buf format` ends comment-alignment drift, and breaking-change detection runs before every commit. The version rides go.mod, so no machine setup beyond `go tool`.
3. **Generate the .proto files themselves from Go types.** The tooling for that direction is weak and it inverts the contract — the schema should constrain the code, not trail it. Rejected without a spike.

## Consequences

- The .proto files stay the one hand-authored artifact, which is inherent to proto-first design: the schema is the API.
- Explicit `go_package` options remain in each file (buf's managed mode could inject them, but flipping it would move the generated import path and churn every import for zero behavior change).
- Generated headers now read `protoc (unknown)` since buf drives the plugins directly — a one-time cosmetic diff.
- protoc itself is no longer needed on PATH, only the two protoc-gen-go plugins buf invokes locally.
