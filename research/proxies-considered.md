# Proxies considered for the per-node data plane

ADR 0007 put Envoy on every node and dismissed Traefik, Caddy, and nginx in a line each. This note
gives the three a fair burial for the decision matrix, checking each against its own docs and
source. The shape they had to fit comes from ADR 0006: every Service owns a VIP per node, VIPs
appear at runtime as Services do, and the instances behind each VIP churn by the second.

## Traefik: dynamic routers, static entrypoints

Traefik splits configuration in two. Routing config (routers, services, middlewares) is dynamic,
and the file and HTTP providers apply changes hot, "without any request interruption or connection
loss". Install config (providers, entrypoints) loads at startup, and the same page files
entrypoints under elements that "don't change often"
([configuration overview](https://doc.traefik.io/traefik/getting-started/configuration-overview/)).
The source is blunter than the docs: entrypoints are built once in `main` from
`staticConfiguration.EntryPoints` (traefik/traefik `cmd/traefik/traefik.go:222`, master as of
2026-08), and the config watcher's only write path into a running entrypoint is `SwitchRouter`
(`pkg/server/server_entrypoint_tcp.go:382`). Dynamic config can't even express a listener, since a
TCP router's `entryPoints` field just names entrypoints that must already exist
(`pkg/config/dynamic/tcp_config.go:74`). So TCP routers arrive at runtime and listen addresses
never do.

Mapping a VIP per (Service, node) onto that leaves two workarounds.

1. Restart Traefik on every Service create and delete, which drops every connection the restart
   wasn't about.
2. Pre-declare a pool of entrypoints on `0.0.0.0` and multiplex. The TCP muxer matches on ALPN,
   ClientIP, HostSNI, and HostSNIRegexp only (`pkg/muxer/tcp/matcher.go:15`), and plain TCP
   without TLS falls through to the `HostSNI(*)` catch-all. Nothing matches on the destination
   address, so two Services holding port 5432 on different VIPs are indistinguishable.

Even with entrypoints solved, Traefik covers only the proxy hop. We'd still run our own DNS to
resolve Service names to VIPs, still allocate and plumb the VIPs onto the bridge, and still
compute policy identity ourselves. The TCP `ipAllowList` middleware takes raw CIDR strings
(`pkg/config/dynamic/tcp_middlewares.go:10`), so identity mapping lands on klited either way.
Traefik v3 did grow active TCP health checks (`pkg/config/dynamic/tcp_config.go:112`), but they
probe from the edge after the fact. There's no drain state, only deletion.

## Caddy: dynamic listeners, wrong update model

ADR 0007's one-liner called listener addresses "static or awkward" for both Traefik and Caddy, and
for Caddy that isn't the failure. Its whole config is one JSON document behind an admin API, and
`POST /load` sets it "overriding any previous configuration" with changes that "incur zero
downtime" ([admin API](https://caddyserver.com/docs/api)). Listen addresses live inside that
document, so Caddy really can grow a new VIP listener at runtime. The static-listener objection
dies here. Three other things killed it.

1. Caddy proper has no L4 app. That lives in [caddy-l4](https://github.com/mholt/caddy-l4), a
   module compiled in with xcaddy and hosted outside the Caddy organization, whose README warns it
   "is still in development. Please expect breaking changes." The repo went untagged from 2020 to
   v0.1.0 in March 2026 and sits at v0.1.2 today. The parts we'd need exist (a `remote_ip` matcher
   for source-CIDR allow and deny at `layer4/matchers.go:122`, active and passive health checks in
   `modules/l4proxy/healthchecks.go`), so the objection is maturity, not features.
2. The update model is document replacement. Every instance start, stop, or readiness flip means
   regenerating the entire config and POSTing it, so we'd write a control loop that diffs desired
   state and emits proxy config. That's an xDS server minus incremental endpoint updates, resource
   versions, and ACK/NACK.
3. Health lives in the wrong place. l4proxy discovers upstream health by probing, each config load
   provisions a fresh app, and `Stop` closes the listeners and walks away (`layer4/app.go:111`).
   Probe state resets on every swap, and nothing external can mark an endpoint draining. klited
   knows an instance is about to die before it dies. caddy-l4 has no way to say so, and EDS says
   it in one field.

Caddy also does nothing about names, so our DNS, VIP allocation, and identity mapping remain ours.

## nginx: reload as the only API

Open-source nginx does L4 through the stream module, configured in files. The upstream docs state
the boundary plainly: `zone`-backed groups can change membership and per-server settings "without
the need of restarting nginx" only "as part of our commercial subscription"
([ngx_stream_upstream_module](https://nginx.org/en/docs/stream/ngx_stream_upstream_module.html)).
Stream health checks and the `state` file sit behind Plus too. OSS picked up the DNS `resolve` parameter in 1.27.3, which tracks a name's records, but that moves
churn into a DNS zone we'd be running anyway. New `listen` sockets still need a reload.

The OSS workflow is templating nginx.conf, running `nginx -s reload`, and repeating per change. A
reload is graceful and heavy at once. The master validates, opens new sockets, and starts a fresh
worker generation, while old workers "close listen sockets and continue to service old clients"
until done ([control docs](https://nginx.org/en/docs/control.html)). Long-lived TCP connections
(databases, gRPC streams) keep old generations alive indefinitely, and the escape hatch is
`worker_shutdown_timeout`, after which nginx "will try to close all the connections currently
open" ([core module](https://nginx.org/en/docs/ngx_core_module.html#worker_shutdown_timeout)). At
per-second churn that means a reload per second. Worker generations stack behind every long-lived
connection, memory multiplies per generation, and connections get cut by a timer because an
unrelated Service scaled. Reload-per-change is a fine shape for config that moves daily. Ours
moves by the second.

## Side by side

|                                  | Traefik                  | Caddy + caddy-l4              | nginx OSS                     | Envoy + xDS (chosen)          |
|----------------------------------|--------------------------|-------------------------------|-------------------------------|-------------------------------|
| Dynamic L4 listeners             | No, entrypoints are startup config | Yes, config load swaps listeners | No, new `listen` needs reload | Yes, LDS adds them live |
| Endpoint churn without reload    | Yes, file or HTTP provider | Yes, but whole-document replace | No, template and reload      | Yes, EDS diffs                |
| Health and drain states          | Active TCP probes, no drain | Local probes, reset per swap | Plus-only for stream          | Pushed per endpoint, with `DRAINING` |
| Policy hook with caller identity | `ipAllowList`, CIDRs we compute | `remote_ip`, CIDRs we compute | `allow`/`deny`, reload to change | RBAC principals pushed by klited |
| Still needs our DNS              | Yes                      | Yes                           | Yes                           | Yes                           |

## Why programmed beat configured

A configured proxy treats its config as a document. You hand over a complete description, it diffs
and applies, and anything the document can't say (this endpoint is draining, that update was
rejected) has no channel to travel in. xDS is a push protocol instead. klited already holds the
authoritative state (which instances exist, which are ready, which are about to die). EDS carries
that state as state, with per-endpoint health that includes a
[`DRAINING`](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/core/v3/health_check.proto)
status, versioned resources, and a NACK when the proxy refuses an update. Building on Traefik or
Caddy meant writing that control loop anyway and then flattening its output into documents that
lose the semantics, while nginx offers nothing to program at all. Envoy is the one candidate
designed to be programmed rather than configured.
