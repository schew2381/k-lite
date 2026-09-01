# The UI shows the internet leg before M9 serves it

M9 is building the cross-machine data plane, and its in-flight protos already settle the shape: remote endpoints dial the owning node's advertised machine address and mTLS ingress port, while the owning node keeps dialing its instances raw. The UI adopts that design now, mock-first. Every EDS row says whether an endpoint is local or reached over the internet, and the inspector shows each remote endpoint's dial target. A traced remote call grows from seven steps to nine:

1. the pick that returns a machine address instead of an instance IP
2. the mTLS leg across the open internet
3. the DNAT hop through the published port into the target's infra pod
4. the raw hand-off to the instance

## Considered Options

1. **Wait for M9 to land.** The visualization work has no dependency on working code, only on the design, and the design is written. Waiting buys nothing except a bigger diff later.
2. **Store per-endpoint ingress ports on Instance objects.** It would make the mapping watchable, but the backend derives ports from each node's `ingress_port_base` and carries them on the NetDesired stream. Inventing storage the backend doesn't have is the `status.infra` mistake again.
3. **Build on M9's own field names, with the port as a sim-only enrichment** (chosen). `Node.status.advertiseAddress` is real and watchable, so both modes read it. The ingress port rode NetDesired at decision time, so the sim stamped it on instance status and the UI degraded to the address alone when it was absent.

## Consequences

- The answer to "where does the mapping live" is split. etcd stores the machine address on `Node.status.advertiseAddress`, and the per-endpoint ingress port stays derived (node port base plus slot), flowing to proxies over NetDesired the way endpoint state itself does.
- Superseded in part the same day: the backend then materialized `IngressAllocation`, a stored kind named `<service>.<instance>` carrying the published port. The UI now reads those objects in both modes, the sim reconciles them beside the VIP allocations, the etcd browser shows the `/klite/v1/ingressallocations` prefix, and the sim-only instance field is gone.
- The mock gives every node its own machine on TEST-NET-2 (`198.51.100.<index>`) and an ingress window at `30000 + (index-1)*512`, so cross-node is cross-machine and every remote call tells the full story. The slot rule is the sim's own until M9's endpoints engine lands.
- Remote picks cost ~30ms in the sim where local ones cost ~5ms, so the latency the rail prints teaches the same lesson the trace does.
- The trace wording follows the proto comments: mTLS between proxies, the published port as the DNAT hop, and the owning node "dialing ip:port raw". The plaintext-on-the-wire caveat from the early outline is gone because M9 pairs published ports with proxy mTLS.
- The last consequence arrived faster than expected: M9 exposed ports to clients as the stored kind above, and nothing outside the seam moved.
