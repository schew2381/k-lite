# Discovery: our own DNS, and a VIP per (Service, Node)

Service names resolve through k-lite's own DNS server (one per node, inside that node's infra pod) to a VIP owned by that same node. Every node holds its own VIP for every Service, and its instances only ever learn the local one. The VIP is where load balancing, readiness, and policy get to stand, and it stays fixed for the Service's lifetime no matter how instances churn.

## Considered Options

1. **Docker network aliases.** Zero infrastructure, since containers sharing an alias round-robin in Docker's built-in DNS. But it gates nothing on readiness (dead containers linger in answers), gives policy no chokepoint, and offers no path to multiple machines. This was the "why not just…" baseline, and it deserved a written burial.
2. **Environment-variable injection.** Addresses go stale on every scale or reschedule, and Workloads acquire start-order dependencies.
3. **DNS round-robin straight to instance IPs.** It skips the proxy hop, but policy has nowhere to stand and client resolver caches (JVMs default to 30 seconds) keep serving the dead.
4. **One cluster-wide VIP per Service.** That's the k8s shape, but all our local nodes share one bridge, so multiple proxies claiming one IP is an ARP fight.
5. **DNS + per-(Service, Node) VIPs** (chosen). Node-local egress is exactly kube-proxy's semantics, so the same design works unchanged on a real Linux node.

## Consequences

- Stale client DNS caches stay *correct*: a cached VIP still works because churn is absorbed behind it.
- IPAM grows a control-plane-allocated VIP pool (10.44.64.0/18), and the answer an instance gets depends on which node asked, by design.
- "A knows B exists" holds even under a deny policy, and ADR 0017 pins that behavior.
