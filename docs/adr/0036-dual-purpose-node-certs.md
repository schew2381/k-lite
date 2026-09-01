# Node certificates carry both TLS purposes

Node certs were ClientAuth-only, which broke 0034's assumption the moment a destination Envoy presented one as an ingress *server* certificate: BoringSSL rejects the chain with "unsuitable certificate purpose" (reproduced live, `fail_verify_error` climbing). `SignNodeCSR` now stamps ClientAuth and ServerAuth together, making the node identity the standard dual-purpose workload certificate — the same shape Istio issues.

## Considered Options

1. **A separate serving cert delivered in the Register response.** Zero CA changes, but the private key would cross the wire — the CSR flow exists precisely so keys never travel — plus a second cert pair to manage and a misleading CN.
2. **Dual-EKU node certs** (chosen). One line in the signer, no existing auth path loosens: the gate keys on the CN prefix and never inspected EKU.

## Consequences

- Identities minted before this change dial fine but can't serve. Agents self-heal: a persisted ServerAuth-less cert triggers a silent re-join when a token is in hand, and a pointed error without one.
- Certificate material folds into the Envoy config hash, since Envoy loads TLS files at resource creation and never re-reads them.
- Recorded trust delta: the source Envoy verifies the serving cert chains to the cluster CA, not that it belongs to the specific node EDS named. klited controls EDS addresses, so this stays inside 0034's node-level trust model — tightening it to per-node SAN matching is the known next step.

## Outcome

verify-m9 starts from wiped state on every run, so the dual-EKU join flow is what each pass proves: `openssl -purpose` shows `SSL server : Yes` on all three fresh node certs, the ingress listeners complete handshakes against node certificates, and a certificate signed by a foreign CA registers as `fail_verify_error` instead of reaching the pod. The self-heal path for pre-M9 identities shipped alongside and stays covered by the agent's join tests.
