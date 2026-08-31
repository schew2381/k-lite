# Consul research for ADR 0007

Sources: a local clone at `~/code/consul` (main @ 681197a, v2.1.0-dev, BUSL-1.1) and the official
docs at developer.hashicorp.com/consul, checked 2026-08-31. Paths are relative to the clone, line
counts exclude `_test.go` files, and the binary number comes from `go build` on darwin/arm64.

## What maps 1:1 onto k-lite

Registration flows through the node-local agent (`PUT /v1/agent/service/register`,
`agent/agent_endpoint.go`), which runs the health checks itself and syncs local state to the
servers through anti-entropy (`agent/ae/ae.go`). The check menu in `agent/checks/` runs script,
TTL, HTTP, H2 ping, TCP, UDP, Docker exec, gRPC, alias, and OS service checks, ten kinds in all.
That subsumes our agent-probed Endpoint readiness (ADR 0003, 0011) with room to spare.

Each agent also serves DNS on 8600 (`agent/dns.go`, `agent/config/default.go:127`). Services
answer with A and SRV records at `[<tag>.]<service>.service[.<dc>.dc].<domain>`, so plain
`redis.service.consul` resolves. Instances that fail their health check or a node system check
"are omitted from the results" (docs: discover/service/static). That's our `<service>.svc.klite`
(ADR 0017) with one real difference. Consul answers with health-filtered instance addresses,
while our DNS hands out a fixed VIP and lets Envoy absorb churn behind it (ADR 0006). Consul
later grew the same idea, with servers allocating per-service virtual IPs for transparent proxy
mode (`agent/consul/state/catalog.go:96-106`). Both systems keep policy out of DNS, so a denied
caller still resolves which is exactly ADR 0017's "existence is public" rule.

## Intentions vs our NetworkPolicy

An intention names a source and destination service and carries either an `Action` (allow or deny,
the L4 form) or L7 `Permissions` (HTTP matchers on path, method, headers), never both per source
(`agent/structs/intention.go:79-87`, `config_entry_intentions.go:806-807`). L4 gets enforced
against the SPIFFE identity in the caller's certificate during the TLS handshake at the
*destination* proxy (`agent/connect/uri_service.go`), and Permissions ride the HTTP filter chain.
Side by side with ADR 0009's model:

| | Consul intentions | k-lite NetworkPolicy (ADR 0009) |
|---|---|---|
| default with no rules | ACL default policy, or `default_intention_policy` (`agent/agent.go:786-802`) | accept everyone |
| adding the first ALLOW | changes nothing for other sources | flips that Service to allowlist mode |
| ALLOW vs DENY conflict | most specific rule wins, deny breaks ties only | DENY always wins |
| priorities | precedence 9 (exact pair) down to 1 (`*` to `*`) (`agent/structs/intention.go:370-390`) | none |
| identity | mTLS certificate SPIFFE ID | control-plane IP-to-Instance map |
| enforcement point | destination proxy, inbound | source node's Envoy |
| L7 | Permissions on HTTP attributes | not modeled, Service pairs only |

Evaluation sorts by precedence and lets the single best match decide (`agent/xds/rbac.go:74-77`).
The worked example in the comment at `agent/xds/rbac.go:543-588` shows the case our model forbids:

```
intern/trusted-app => billing/payment-svc : ALLOW (prec=9)
intern/*           => billing/payment-svc : DENY  (prec=8)
*/*                => billing/payment-svc : ALLOW (prec=7)
::: ACL default policy :::                : DENY  (prec=N/A)
```

Under ADR 0009 the `intern/*` DENY would end the story, because no ALLOW can reopen what a DENY
closed. Here the exact pair outranks it and `intern/trusted-app` gets through. Neither reading is
wrong. Consul behaves like firewall rules with auto-computed priorities, and ours copies Istio's
deny-overrides. The two aren't translations of each other, so "mirror NetworkPolicies into
intentions" goes lossy in precisely the corner where both of our canonical policy examples live.

## What running it costs

- A raft quorum of servers comes first (`hashicorp/raft v1.7.3` in go.mod), "either 3 or 5 servers
  for production deployments" (docs: concept/consensus). The production reference architecture
  asks for 5 nodes across three availability zones at 8-16 cores and 32-64 GB each. Beside our
  etcd (ADR 0005) that's a second consensus cluster to feed, back up, and page on.
- Every node runs a client agent joined to the LAN gossip pool on 8301 for membership and failure
  detection, with a WAN pool on 8302 held for federation (`hashicorp/serf v0.10.4` over
  `hashicorp/memberlist v0.6.0`, docs: concept/gossip). k-lite has no gossip layer anywhere, so
  this arrives as pure addition.
- It brings its own security machinery, ACL tokens and policies (`acl/`, about 4k lines) plus a
  Connect CA with built-in, Vault (six auth flavors), and AWS PCA providers (`agent/connect/ca/`),
  and we'd run all of that beside ADR 0013's node mTLS rather than instead of it.
- The agent manages sidecars itself. It registers one proxy per service
  (`agent/sidecar_service.go`), `consul connect envoy` renders a bootstrap and execs the binary
  (`command/connect/envoy/`), and the agent serves xDS on 8502 from the 15,020 lines of
  `agent/xds/`, 2.4 times all of k-lite today.
- The default port map runs 8300 (server RPC), 8301/8302 (serf), 8500 (HTTP), 8502 (xDS), 8600
  (DNS), and a reserved 21000-21255 sidecar range (`agent/config/default.go:126-141`).

Measured on the clone, the repo runs 96 MB shallow, 2,411 Go files, 747k lines with tests and 357k
without tests or the 22 MB Ember UI. `agent/` alone holds 201k non-test lines against k-lite's
current 6,138 across 13 files. go.mod requires 286 modules, 175 of them indirect, among them raft,
serf, memberlist, and five go-control-plane modules. The build produced a 178 MB unstripped
binary, and the license is BUSL-1.1, with file headers now reading "Copyright IBM Corp".

## The integration we'd have built

klited would keep etcd as desired state and grow a sync loop mirroring every Service and ready
Endpoint into Consul's catalog, either through the server APIs or a client agent added to each
infra pod. NetworkPolicies would compile to `service-intentions` config entries through the lossy
translation above, or we'd adopt intention semantics and give up "DENY always wins". Envoy still
runs on every node, but `consul connect envoy` bootstraps it against the agent's 8502, so the xDS
server, the discovery state machine, and the policy evaluator all move out of klited. DNS moves to
8600, where either the `consul` domain leaks into Workloads or CoreDNS forwards `svc.klite` at it,
and ADR 0006's per-node VIP semantics would need rebuilding on Consul's transparent-proxy virtual
IPs. What remains of k-lite is the scheduler, the Docker lifecycle, drain and surge, the UI, and a
translator shuttling state between two sources of truth. That sync loop is exactly the kind of
glue that teaches nothing and still wakes you at night.

## Verdict for ADR 0007

The rejection holds, and the numbers make it sharper. Consul earns its footprint when the problems
it uniquely solves are real. WAN federation is wired in (the 8302 pool exists for it), one catalog
can span a mixed VM-and-Kubernetes estate, and an ops team gets a supported product with an ACL
story rather than control-plane code to own. Its architecture even validates ours, since Consul is
itself a go-control-plane xDS server programming Envoy. But k-lite exists to build that control
plane, and the spike already proved our miniature version works end to end (ADR 0007,
`hack/spike-envoy/`). Adopting Consul would hand the 15k-line lesson to a vendor and keep the
homework of a raft quorum, a gossip mesh, and an agent per node. We'd operate 357k lines of
somebody else's Go when the point was writing 6k of our own.
