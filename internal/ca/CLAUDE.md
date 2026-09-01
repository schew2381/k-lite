# internal/ca

Certificate authority, join tokens, and TLS config builders for node identity (ADR 0013, research/join-auth.md). Pure crypto and encoding — persistence layout, liveness checks, and one-time-token bookkeeping belong to the callers.

Invariants:

- `ServerCert` returns the chain as [leaf, CA] and bootstrap pinning depends on that order. A listener presenting only the leaf breaks trust-on-first-use.
- CSRs must pass `CheckSignature`, and every name they carry gets ignored — the server stamps `CN=klite:node:<name>` itself.
- Token and hash comparisons are constant-time. Keep them that way.
- The bootstrap verifier checks both the pinned CA hash AND that the leaf chains to it, which is what defeats replay. Resumed sessions re-verify via VerifyConnection.
