# Nodes join with a token, then hold an mTLS certificate

`klited` mints its own CA at first boot. A joining agent presents a one-time join token (from `klite node token`), submits a CSR, and receives a client certificate binding its node name. Every gRPC call after that is mutually authenticated, and the node's Envoy uses the same credentials for its xDS stream. The CLI stays on a local admin token.

## Considered Options

1. **Bearer node tokens forever.** Simpler to build, but a secret string is all an attacker needs. Revocation is a config edit someone forgets.
2. **One shared cluster secret.** Demo-grade, with no per-node identity at all.
3. **Token bootstrap into mTLS** (chosen). The flow is k3s-shaped. Identity becomes a certificate the transport itself verifies, and the future service-to-service mTLS (ADR 0009's recorded delta) would reuse this CA and these certificates.

## Consequences

- klited carries CA code that issues, verifies, and stores under `~/.klite/server/tls`.
- Certificate rotation is out of scope for v1, written here so nobody mistakes the omission for an accident.
- The WAN posture falls out for free, since a remote node needs only the server address, a token, and an outbound connection. Nothing on the node listens.
