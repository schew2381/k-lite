# Istio and Linkerd: why we cite them instead of running them

ADR 0007 dismissed both meshes in one sentence, "both effectively require Kubernetes underneath,"
and pointed here for the receipts. ADR 0009 then copied Istio's authorization semantics anyway,
so this note substantiates the first claim, cites the model behind the second, and records where
k-lite knowingly deviates. Sources are the official docs and istio/istio, read 2026-08-31.

## Why neither fits k-lite

### Linkerd

Linkerd doesn't hide it. The [FAQ](https://linkerd.io/faq/) grants that "Linkerd has certain
technical prerequisites, such as Kubernetes," and the
[getting-started guide](https://linkerd.io/2-edge/getting-started/) checks for a Kubernetes
cluster and a working `kubectl` before anything else. Installation pipes `linkerd install --crds`
and then `linkerd install` into kubectl, delivering the Custom Resource Definitions and then the
control plane. The [architecture reference](https://linkerd.io/2-edge/reference/architecture/)
calls that "a set of services that run in a dedicated Kubernetes namespace (`linkerd` by
default)". Discovery, identity, and the injection webhook all run there.

[Mesh expansion](https://linkerd.io/2-edge/tasks/adding-non-kubernetes-workloads/) (added in
2.15) doesn't change that. A VM joins by pointing its proxy at control-plane addresses like
`linkerd-dst-headless.linkerd.svc.cluster.local`, names only a cluster can serve, and it gets
registered "via an `ExternalWorkload` CRD that needs to be present in the cluster". Even the VM
path runs through the cluster.

### Istio

Istio needs the honest version, because its docs never say "requires Kubernetes" and the
[architecture page](https://istio.io/latest/docs/ops/deployment/architecture/) still claims
"Istio can support discovery for multiple environments such as Kubernetes or VMs." Every
supported path routes through a cluster anyway:

- Configuration is CRDs. "Like other Istio configuration, the API is specified using Kubernetes
  custom resource definitions (CRDs)"
  ([traffic management](https://istio.io/latest/docs/concepts/traffic-management/)). Without an
  API server there's nowhere to put a VirtualService.
- Discovery reads the platform: "To populate its own service registry, Istio connects to a
  service discovery system," with a Kubernetes cluster as the worked example (same page).
- [VM support](https://istio.io/latest/docs/setup/install/virtual-machine/) begins with "Install
  Istio and expose the control plane on cluster so that your virtual machine can access it," and
  the VM's WorkloadGroup gets pushed to the cluster. Mesh expansion extends a cluster rather than
  replacing one.
- The escape hatches rotted away. The last in-tree non-Kubernetes registry, Consul, was deleted
  for 1.8 ([istio/istio#25833](https://github.com/istio/istio/pull/25833)), and every release in
  the [support matrix](https://istio.io/latest/docs/releases/supported-releases/) is qualified
  against "Supported Kubernetes Versions" alone.

ADR 0007's wording overstates the letter of Istio's docs and states their practice exactly,
since istiod treats the cluster as config store, registry, and trust plumbing. Adopting either
mesh means running Kubernetes to avoid running Kubernetes.

## The AuthorizationPolicy model we copied

ADR 0009's evaluator is Istio's, minus the CUSTOM and AUDIT actions. The
[reference](https://istio.io/latest/docs/reference/config/security/authorization-policy/) fixes
the order: "When CUSTOM, DENY and ALLOW actions are used for a workload at the same time, the
CUSTOM action is evaluated first, then the DENY action, and finally the ALLOW action." Its
evaluation rules, condensed:

1. A matching CUSTOM policy whose engine says deny denies the request.
2. Any matching DENY policy denies the request.
3. "If there are no ALLOW policies for the workload, allow the request."
4. A matching ALLOW policy allows it, and otherwise the request is denied.

Rules 3 and 4 carry the flip we kept. A workload that no ALLOW policy targets accepts everyone,
and deny-by-default switches on only once an ALLOW policy targets it. The [security concepts
page](https://istio.io/latest/docs/concepts/security/#authorization) confirms both halves and
adds the enforcement point, "the authorization policy enforces access control to the inbound
traffic in the server side Envoy proxy." The caller's name arrives on the wire, since with
mutual TLS "Istio extracts the identity from the peer authentication into the `source.principal`."

Linkerd lands on the same shape with different nouns
([authorization policy](https://linkerd.io/2-edge/features/server-policy/)). By default it
"allows all traffic to transit the mesh." A `Server` resource selecting a pod's port flips that
port to deny-unless-authorized, and `AuthorizationPolicy` plus `MeshTLSAuthentication` grant
access by the client's TLS identity, checked by the receiving pod's proxy. Both meshes default
to allow until a policy lands, then enforce at the server, keyed on mTLS peer identity.

## What we deliberately do differently

k-lite enforces in the RBAC filter at the source node's Envoy, not the destination's, because our
destination listener would see the source Envoy's address instead of the caller's. Identity comes
from the control plane's IP-to-Instance map rather than from a certificate presented on the
connection. ADR 0009 records both deltas along with the hole they open. A caller that learns a
raw instance IP out of band skips our policy entirely, while a mesh's server-side proxy still
sits in that path and demands a peer certificate whatever address was dialed. Identity that
travels with the connection is what lets the meshes enforce at the server. Service-to-service
mTLS is our recorded future fix, reusing the CA and certificates ADR 0013 already issues.

## The ambient-mode parallel

Istio's second data plane converged on k-lite's shape. Ambient mode drops sidecars for ztunnel,
a per-node proxy carrying mTLS, telemetry, and L4 authorization for every meshed pod on its node
("there is a single instance of the ztunnel proxy on each node," per the
[ambient data plane docs](https://istio.io/latest/docs/ambient/architecture/data-plane/)).
ADR 0008's infra pod makes the same bet, one shared network-plumbing point per node (klite-net
plus Envoy in one netns) instead of a sidecar per instance. The difference sits above L4, where ztunnel refuses
HTTP and delegates it to waypoint proxies while k-lite's node Envoy carries L7 itself, closer to
a waypoint pinned one per node.
