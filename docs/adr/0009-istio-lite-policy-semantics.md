# NetworkPolicy speaks Istio's model, scaled down

A policy carries an action, ALLOW or DENY, and rules between Service names. Evaluation copies Istio's AuthorizationPolicy. DENY rules always win. A Service that no ALLOW policy targets accepts everyone. The first ALLOW that targets it flips that Service to allowlist mode. There are no priority integers. Enforcement happens in the RBAC filter at the *source* node's Envoy, with caller identity derived from the control plane's IP-to-Instance map.

## Considered Options

1. **Priority-ordered rules.** Round 1 recommended this. It's expressive, but priority numbers invite shadowing bugs and composition stays manual.
2. **k8s NetworkPolicy semantics.** It's allowlist-only with no deny verb, and it's famously the most misread API in Kubernetes. It can't state "A talks to everything except B" without contortions.
3. **Istio's model minus CUSTOM and AUDIT** (chosen). Both of this project's canonical examples ("A cannot talk to B", "A can talk to everything except B") are a single DENY rule, and adding an ALLOW can never accidentally reopen traffic a DENY closed.

## Consequences

- The evaluator is a pure function, unit-tested once and reused by the xDS RBAC compiler, the PolicyCheck RPC, and the UI simulator.
- Three deltas from Istio are accepted and written down:
  - enforcement at the source rather than the destination, because the destination would see the proxy's IP instead of the caller's
  - identity from an IP map rather than mTLS certificates
  - a raw-instance-IP bypass for callers that learn addresses out of band
- Service-to-service mTLS is the recorded future fix for the last two.
- DNS still resolves denied destinations (ADR 0017), which pins that existence stays public even when reachability isn't.
