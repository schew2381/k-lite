# One TLS listener, and every RPC passes a deny-by-default gate

klited serves everything — ClusterService, AgentService, ADS — on a single TLS 1.3 listener that requests but doesn't require client certificates. An interceptor then classifies every call: a node certificate opens AgentService and xDS (with the node name checked against the cert on per-node RPCs), the admin bearer token opens ClusterService, and Register alone is reachable with only the cluster token. Anything unclassified is denied.

## Considered Options

1. **Two listeners** — one mTLS-required for agents, one token-only for clients. The trust math is identical, and it doubles the ports, certs plumbing, and failure modes.
2. **Ad-hoc checks inside each RPC.** The pattern that rots: the next RPC added forgets its check and ships open.
3. **Single listener, VerifyClientCertIfGiven, one deny-by-default interceptor** (chosen). New services must claim a caller class in auth.go or stay unreachable, which turns forgetting into an outage instead of a hole.

## Consequences

- The CLI's `--insecure` skips certificate verification only, never encryption.
- The admin token mints with O_EXCL so two replicas racing first boot can't both create it.
- Recorded residual: xDS snapshots aren't yet bound to the certificate's node identity, so any certified node can read another's snapshot. M9 closes this while it's in the cross-node security business.
