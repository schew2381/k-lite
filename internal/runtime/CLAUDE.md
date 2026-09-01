# internal/runtime

The Runtime interface and its Docker implementation. This is the blast shield from ADR 0019: the moby SDK is v0.x and breaks between minors, so this package is the ONLY importer — everything else sees our interface.

Invariants:

- Containers are named `klite.<node>.<name>` and carry the `io.klite.*` labels. Those labels are an agent's entire worldview, so label discipline is load-bearing (ADR 0003).
- Workload containers always run with restart policy "no" — the agent owns restarts (ADR 0011).
- Workload containers get the DNS trio `--dns <klite-net IP> --dns-search svc.klite --dns-opt ndots:1`. The ndots option is not optional, musl resolvers break without it (ADR 0008).
- klite0's IPAM (10.44.0.0/16, gateway 10.44.0.1, ip-range 10.44.128.0/17) is validate-or-fail, never silently adopted.
- Netns-scoped options (add-host, NET_ADMIN, static IP, published ports) go on the donor container. Docker rejects them on a container joining another's network.
- The daemon socket resolves DOCKER_HOST first, then the colima path. The SDK ignores docker CLI contexts.
