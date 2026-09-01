# api/proto

The wire contract and the project's source of truth (ADR 0004): objects, ClusterService (CLI and the user's facade), AgentService (nodes), KliteNetService (agent → klite-net). Generated Go lives in `internal/gen`, committed.

Rules:

- `make proto` formats and generates through buf (ADR 0021). `make proto-lint` adds `buf breaking --against main` — run it before changing any message.
- Field changes are additive only: new field numbers, never renumber, never retype. Enums keep their UNSPECIFIED zero.
- Stream RPCs answer with semantic messages (DesiredState, Command, LogChunk) rather than *Response wrappers — buf.yaml excepts those two lint rules on purpose.
- A change to RPC or field semantics is an architecture change. It gets an ADR.
