# Join auth research for M8 node identity (feeds ADR 0013)

Sources: k3s checkout at `~/code/k3s` (files cited by path), docs.k3s.io, the kubelet TLS bootstrapping page on kubernetes.io, `go doc crypto/x509`, envoy protos in `~/code/go-control-plane`.

## The k3s token, and why the CA hash rides in it

A full k3s agent token reads `K10<64 hex chars>::node:<password>`. The prefix and hash length sit in
`pkg/clientaccess/token.go:28-29` (`tokenPrefix = "K10"`, `caHashLength = sha256.Size * 2`), the
credentials come from `pkg/daemons/control/deps/deps.go:263` (`EnsureUser("node", "k3s:agent", ...)` with
a random 16-byte password), and `FormatTokenBytes` (`token.go:572`) glues the parts together. The hex
blob is the SHA-256 of the cluster CA (`hashCA`, `token.go:165`), hashing the literal PEM bytes of a
single self-signed cert, or the DER of the root when the bundle holds intermediates.

The hash exists because the server's CA is private, which leaves a joining agent with nothing in its
trust store that can verify the server's TLS cert. k3s downloads the CA bundle from `/cacerts` over
deliberately unverified TLS (`getCACerts`, `token.go:412-439`, an `InsecureSkipVerify` client), compares
its digest against the one pinned in the token (`validateCAHash`, `token.go:391-407`), and aborts on
mismatch. That's trust-on-first-use with the trust carried out-of-band in the token, so impersonating
the server means colliding with the pinned digest. Later requests pin `RootCAs` to that CA
(`GetHTTPClient`, `token.go:267`). Kubelet TLS bootstrapping is the same idea in Kubernetes clothes, a
`system:bootstrappers` token redeemed through the CSR API for a `CN=system:node:<name>`,
`O=system:nodes` cert.

## The handshake k-lite copies

k3s spreads this across HTTP handlers (`pkg/server/handlers/router.go:36-38`) and the agent's
`pkg/agent/config/config.go`. The k-lite version, as gRPC on the one klited listener:

1. The operator runs `klite node token`, which emits `K10<sha256(ca.crt)>::<id>:<secret>` and stores
   the secret's hash in etcd as a one-time record.
2. The agent parses the token, fetches the CA PEM over an `InsecureSkipVerify` dial via a
   `Bootstrap.GetCACert` RPC (the `/cacerts` analog), hashes it, and aborts on a pin mismatch.
3. The agent generates `~/.klite/agent/<node>/tls/node.key` (0600) and an empty CSR. k3s sends
   `x509.CreateCertificateRequest(rand, &x509.CertificateRequest{}, key)` (`config.go:318-328`), because
   the CSR is only an envelope proving possession of the public key.
4. The agent redials with `RootCAs` pinned to the fetched CA and calls `Bootstrap.Join(token, csr, nodeName)`.
5. klited validates the token against the etcd record, marks it used, rejects a name a live node
   already claims, and signs a cert, stamping the identity itself (`CN=klite:node:<name>`,
   `O=klite:nodes`, client-auth usage). k3s stamps the same way in `ClientKubeletCert`
   (`handlers.go:93-106`) and takes nothing from the CSR but its public key (`csrSigner`,
   `handlers.go:216-228`), so names a client writes into its CSR never survive.
6. The agent writes `node.crt` and `ca.crt` beside the key. k3s writes the same trio as 0600 files
   under `<data-dir>/agent/` (`config.go:443-445,367-370`).
7. Every later gRPC connection presents the cert, and on restart the agent finds the tls dir and skips
   joining, the reuse k3s gets from `clientaccess.WithClientCertificate` (`config.go:445`).

k3s also binds each name with an immutable per-node password secret created on first join
(`pkg/nodepassword/nodepassword.go:49-87`), so a stolen token can't re-register an existing node's
name. One-time tokens plus the claim check in step 5 buy k-lite the same property with less state.

## Go sketches for the parts k-lite writes itself

Minting the CA and signing a node cert are the same call, self-signed when template == parent:

```go
ca := &x509.Certificate{
    SerialNumber: serial, Subject: pkix.Name{CommonName: "klite-ca@" + ts},
    NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
    KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
}
caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
csr, err := x509.ParseCertificateRequest(csrDER) // then csr.CheckSignature()
leaf := &x509.Certificate{
    SerialNumber: serial, NotAfter: now.AddDate(1, 0, 0),
    Subject:     pkix.Name{CommonName: "klite:node:" + name, Organization: []string{"klite:nodes"}},
    KeyUsage:    x509.KeyUsageDigitalSignature,
    ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
}
der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, csr.PublicKey, caKey)
```

k3s parses the CSR but never calls `CheckSignature` (`handlers.go:244-246`, no hits in `pkg/`). Keep
it, since proof-of-possession costs one line. The grpc research chose a single listener with dual auth,
so the server side is

```go
creds := credentials.NewTLS(&tls.Config{
    Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: caPool,
})
```

plus the deny-by-default interceptor from `research/grpc-go.md`. Non-empty `VerifiedChains` yields
principal `node:<name>` from the CN prefix, a bearer token yields `user:admin`, and every method not
explicitly opened to a principal class is denied. Only `Bootstrap.GetCACert` and `Bootstrap.Join`
accept unauthenticated callers, with Join checking the token in the handler. The agent mirrors it with
`credentials.NewTLS(&tls.Config{RootCAs: caPool, Certificates: ...})` after
`tls.LoadX509KeyPair(dir+"/node.crt", dir+"/node.key")`.

## Envoy rides the same identity

The agent already renders a per-node bootstrap from `sample/bootstrap-ads.yaml` (`research/envoy-xds.md`).
M8 adds a mount and a stanza. Run the infra pod's Envoy with
`-v ~/.klite/agent/<node>/tls:/etc/klite/tls:ro`, and give `xds_cluster` a `transport_socket`
(`envoy.transport_sockets.tls`) whose `UpstreamTlsContext.common_tls_context` carries `tls_certificates`
with `certificate_chain` and `private_key` filenames plus `validation_context.trusted_ca` (fields:
`tls/v3/tls.pb.go:94`, `common.pb.go:587,593,982` under `~/code/go-control-plane/envoy/`). klited's
server cert then needs SANs covering whatever Envoy dials, `host.docker.internal` included. The xDS
stream arrives as principal `node:<name>`, so the snapshot server should refuse a mismatched `node.id`.

## Rotation and revocation

k3s issues 365-day leaf certs under a 10-year CA and renews at startup anything expired or within
`CertificateRenewDays = 120` of expiry (`pkg/daemons/config/types.go:30`, `deps.go:628,757-762`,
docs.k3s.io/cli/certificate), with manual `k3s certificate rotate` and `rotate-ca` on top
(`pkg/cli/cert/cert.go:251,365`). ADR 0013 records all of that as out of scope for k-lite v1, and short
lifetimes should stay out too, because a short cert without a renewal path is a scheduled outage. Mint
1-year leaves under a 10-year CA and move on.

The cheap thing worth doing is revocation-by-liveness. k3s rejects cert auth for nodes that no longer
exist (`verifyNode`, `nodepassword.go:91-98`), and k-lite's interceptor already resolves a node
principal per call, so one etcd registry lookup turns `klite node delete` into effective revocation.
The deny-list is the node table we already maintain, so CRL and OCSP machinery stays out entirely.

## M8 checklist

1. Build `internal/pki` with keygen, CA minting, `SignNodeCert(pub, name)`, PEM read/write under
   `~/.klite/server/tls/` (0600 keys), and klited's server cert with the SAN list above.
2. Port token format and parse (`FormatTokenBytes`/`parseToken` shapes), add the `klite node token`
   subcommand, and store one-time token records in etcd.
3. Add the `Bootstrap` gRPC service with `GetCACert` and `Join` (validate token, `CheckSignature`,
   sign, return cert plus CA), registered exempt from the auth interceptor.
4. Write the agent join client (unverified fetch, hash pin, CSR, `Join`) persisting
   `~/.klite/agent/<node>/tls/{node.key,node.crt,ca.crt}` and reusing it on restart.
5. Swap klited's listener to `credentials.NewTLS` with `VerifyClientCertIfGiven`, and extend the auth
   interceptor with cert principals plus the deleted-node check.
6. Point every agent dial at the mTLS creds, leaving the CLI on its local admin bearer token.
7. Extend the Envoy bootstrap template with the tls transport_socket, mount the tls dir read-only into
   the infra pod, and enforce node.id == cert name in the xDS server.
8. Test token roundtrip and hash-mismatch abort as unit tests, then join-then-call and deleted-node
   rejection behind `-tags=integration`.
