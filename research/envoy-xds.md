# Envoy + go-control-plane as k-lite's service data plane

This note feeds ADR 0007 (one Envoy per node, programmed over xDS). It draws on the go-control-plane
checkout at `~/code/go-control-plane`, the Envoy v3 docs, and live tests here (colima 0.10.3, aarch64).

## What it is

Envoy is a C++ L4/L7 proxy that takes its whole runtime config over gRPC (xDS). A control plane
pushes Listeners (LDS), Clusters (CDS), and endpoint assignments (EDS), and ADS multiplexes all
three over one stream. go-control-plane is the official Go server side, with generated protos for
every resource plus a cache and gRPC handler (`pkg/cache/v3`, `pkg/server/v3`). The core object is
`SnapshotCache` (`pkg/cache/v3/simple.go:142`). You build a snapshot (a version string plus a map
of resource type to resources), call `SetSnapshot(ctx, nodeID, snap)` (`simple.go:264`), and the
cache pushes the diff to the Envoy that presented that node id. `cache.IDHash{}` keys on `node.id`
(`pkg/cache/v3/status.go:27`), so k-lite sets `node.id` to the node name and per-node config is
automatic.

The reference server, `internal/example/` (443 lines), pairs with `sample/bootstrap-ads.yaml`.

- `main/main.go` (71) wires `NewSnapshotCache(false, cache.IDHash{}, logger)`,
  `snapshot.Consistent()`, `SetSnapshot`, and `server.NewServer(ctx, cache, cb)`.
- `server.go` (142) registers ADS plus the per-type xDS services on one `grpc.Server`, with
  keepalives tuned for proxies.
- `resource.go` (180) builds an HTTP listener/route/cluster set.

### Prior art: Istio ambient's ztunnel

Istio ambient mode already ships this shape. One ztunnel proxy runs per node as a DaemonSet, not a
sidecar, and handles mTLS, telemetry, and authorization policy at L4 for every pod on the node.
Traffic is "transparently redirected to the node-local ztunnel" by in-pod network-namespace
redirection, so pods need no injection. L7 policy goes to separate waypoint Envoys
(istio.io/latest/docs/ambient/architecture/data-plane/). Per-node L4 proxies under a central
control plane are a production pattern.

## What k-lite needs from it

Each node gets one snapshot, and every service VIP contributes one Listener, one Cluster, and one
ClusterLoadAssignment. The LDS Listener binds the VIP and runs RBAC ahead of `tcp_proxy`:

```yaml
name: svc/db
address: { socket_address: { address: 10.96.0.12, port_value: 5432 } }
freebind: true          # bind VIP before it exists on any interface
filter_chains:
- filters:
  - name: envoy.filters.network.rbac
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.network.rbac.v3.RBAC
      stat_prefix: svc_db
      rules:
        action: ALLOW   # deny anything no policy matches
        policies:
          clients:
            permissions: [ {any: true} ]
            principals: [ {direct_remote_ip: {address_prefix: 10.88.1.0, prefix_len: 24}} ]
  - name: envoy.filters.network.tcp_proxy
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
      stat_prefix: svc_db
      cluster: svc/db
      idle_timeout: 0s  # default is 1h, which silently kills idle DB connections
```

`freebind` sets `IP_FREEBIND`, meaning "listeners can be bound to an IP address that is not
configured on the system running Envoy" (listener.proto). The agent can therefore program a VIP
listener before routing exists, though getting packets to Envoy's netns stays k-lite's problem.
The CDS Cluster is EDS-typed:

```yaml
name: svc/db
type: EDS
connect_timeout: 5s
eds_cluster_config: { eds_config: { ads: {}, resource_api_version: V3 } }
common_lb_config: { healthy_panic_threshold: { value: 0 } }
```

The EDS ClusterLoadAssignment carries per-endpoint health:

```yaml
cluster_name: svc/db          # must equal the Cluster name
endpoints:
- lb_endpoints:
  - { endpoint: {address: {socket_address: {address: 10.88.1.7, port_value: 5432}}}, health_status: HEALTHY }
  - { endpoint: {address: {socket_address: {address: 10.88.2.3, port_value: 5432}}}, health_status: DRAINING }
```

`HealthStatus.DRAINING` "is interpreted by Envoy as UNHEALTHY" (core/v3/health_check.proto), so the
balancer stops picking a draining endpoint. `tcp_proxy` consults the balancer only at connection
setup, so established connections continue. Envoy closes them early only if
`Cluster.close_connections_on_host_health_failure` is set, and removal tears them down only if
`CommonLbConfig.close_connections_on_host_set_change` is set. Both default to false (cluster.proto),
so the pod-stop flow is DRAINING first, a grace period, then removal. `health_status: UNKNOWN` (the
default) counts as healthy, so set it explicitly.

Listener removal drains rather than snaps. Per the LDS docs, "Connections owned by the listener
will be gracefully closed (if possible) for some period of time before the listener is removed",
with the window set by `--drain-time-s` (default 600s, operations/cli). Updates that only touch
filter chains drain just the modified chains, and the socket keeps accepting.

The RBAC network filter (extensions/filters/network/rbac/v3) takes an `action` of ALLOW or DENY and
matches `policies` with OR semantics. Principals match by `direct_remote_ip` ("always the physical
peer") or `remote_ip` (may be inferred from proxy protocol). Enforcement happens once, at the first
byte of data (`enforcement_type` defaults to `ONE_TIME_ON_FIRST_BYTE`). Rule updates ride an LDS
filter-chain update: new connections get new rules immediately, existing connections on the changed
chain are drained — hitless for the service, lossy for those connections. `Filter.config_discovery`
(ECDS) could avoid that drain, but it is not worth the extra resource type in M4.

The per-node bootstrap copies `sample/bootstrap-ads.yaml`, sets `node.id` to the node name, points
`xds_cluster` at `host.docker.internal:18000`, and puts admin on 0.0.0.0:9901. Admin `/config_dump`
shows the applied config (`?include_eds` adds endpoints) and `/ready` gives 200 (operations/admin).

## Integration cost

The M4 xDS server should land around 250-350 lines of Go, split roughly as ~60 for gRPC setup and
service registration (copy `internal/example/server.go`), ~150 for resource builders (tcp_proxy is
simpler than the example's HTTP stack, and RDS drops out entirely), and ~50 to assemble and version
per-node snapshots. The concepts are snapshot versioning (bump the version or nothing propagates),
consistency (`Consistent()` checks that clusters' EDS references are present), and node-id keying.
It adds two Go module deps, `github.com/envoyproxy/go-control-plane` and its `.../envoy` protos.

The `envoyproxy/envoy` image tags are multi-arch. `docker manifest inspect envoyproxy/envoy:v1.35.0`
lists linux/amd64 and linux/arm64, and v1.38.3 is current stable (endoflife.date/envoy). I measured
223MB uncompressed, and it runs here (`envoy version: .../1.35.0/Clean/RELEASE/BoringSSL`). A
container reached a mac-host listener as `host.docker.internal`, with and without
`--add-host host.docker.internal:host-gateway`, and as `192.168.5.2` (the Lima gateway). On older
colima, `host-gateway` resolved to the Lima VM, not the mac (colima#277), so pin colima at 0.10.3
and keep `192.168.5.2` as fallback.

## Risks and gotchas

The panic threshold is the trap that breaks our drain semantics. Below 50% healthy endpoints (the
default), Envoy "will disregard health status and balance either amongst all hosts or no hosts"
(arch_overview panic_threshold). Drain one endpoint of two and it keeps taking new connections.
Set `healthy_panic_threshold: {value: 0}` on every cluster, no exceptions.

The rest cost one line each. `tcp_proxy.idle_timeout` defaults to one hour, so set `0s`.
`--drain-time-s` defaults to 600s, leaving a removed VIP listener up for ten minutes, so run Envoy
with ~15s. RBAC denies fire at first byte, not at accept, so a blocked client sees
connect-then-close. EDS updates merge over a 1s `update_merge_window` by default, visible in tests.

## What to copy and what to avoid

Copy the `internal/example` wiring (cache construction, the `Consistent()` check, service
registration, gRPC keepalives), `sample/bootstrap-ads.yaml` with its
`set_node_on_first_message_only` flag, and the `pkg/resource/v3` type constants. Avoid four things.

- `pkg/test/v3.Callbacks` is a test helper. Write a small logging callback instead.
- The example's HTTP stack (HCM, RDS, LOGICAL_DNS cluster). k-lite needs tcp_proxy only.
- Delta-xDS and `LinearCache`. Plain snapshots are enough at tens of services.
- Any cluster lacking a zero panic threshold.

## Verdict for ADR 0007

The decision holds up. Every k-lite requirement maps to a documented field: VIP pre-binding is
`freebind`, source-IP policy is the network RBAC filter, and drain-then-remove is EDS `DRAINING`
plus two default-off connection-closing knobs. The control plane is a few hundred lines on
`SnapshotCache`, keyed by node name as ADR 0007 assumes. The arm64/colima path is validated, with
the image running here and the control plane reachable three ways. Proceed with Envoy, keeping the
Go proxy fallback for the one risk it cannot solve, routing VIP traffic into its container. The
panic-threshold and drain-time footguns land in M4's checklist.
