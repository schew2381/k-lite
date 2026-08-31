# A local node is an agent process, not a VM

Locally, each cluster node is one `klite-agent` process on the Mac, and all of them share the single colima Docker daemon. Containers carry an `io.klite.node=<name>` label, and each agent only touches its own. A cloud node later is the same binary on a real machine, driving Docker through the same Runtime interface.

## Considered Options

1. **One agent process per node, one shared dockerd** (chosen). It's cheap enough to run five of, fast to add and kill during demos, and honest about the interface, since nothing an agent does assumes it shares a machine with the control plane.
2. **Docker-in-Docker per node.** It gives truer isolation with separate daemons and bridges, but costs privileged containers, slow startup, and nested networking that misbehaves on macOS.
3. **One agent simulating N nodes.** The least code, and also a fiction for the scheduler and drain logic to exercise.

## Consequences

- All local instances share one bridge, so cross-node traffic is trivially flat. ADR 0016 records exactly what changes once nodes run on separate machines.
- Label discipline is load-bearing: `docker ps --filter label=io.klite.node=x` is a node's entire worldview.
- Adding or removing a node is starting or killing a process, visible within one heartbeat interval.
