# Docker networking on macOS/colima: what the k-lite data path stands on

We checked every claim on this machine on 2026-08-31 (colima, vz/aarch64,
Docker Engine 29.5.2) except where a line cites docs instead. Experiments ran on a
throwaway network shaped like `klite0` (`--subnet 172.30.0.0/16 --ip-range
172.30.128.0/17`) with `rsrch-*` containers, all removed afterward.

## 1. Embedded DNS at 127.0.0.11 and the single-upstream rule

Every container on a user-defined network gets `nameserver 127.0.0.11` in
`/etc/resolv.conf`, with or without `--dns` (the default bridge gets none of
this). The embedded server natively resolves container names, aliases
(single-label `b` and dotted `api.rsrch.internal` both answered), and
container IDs, then forwards everything else upstream. `--dns 172.30.0.53`
only swapped the forwarding target, listed in the generated comments as
`ExtServers`. With no `--dns` at all, the upstream is the VM's resolver,
192.168.5.1 (docs.docker.com/engine/network/#dns-services).

A name only our test dnsmasq knew (`myapp.test`) resolved under
`--dns <dnsmasq> --dns 1.1.1.1` and got NXDOMAIN with the order flipped. The
first server's NXDOMAIN is final, and the second never gets asked. Failover
happens only on timeout, and a black-holed first upstream stretched a 0.2s
lookup to 6.5s. A namespace can't be split across two upstreams, and the
first one had better answer.

k-lite relies on this for: making klite-net each instance's sole `--dns`
upstream so nothing races NXDOMAINs against it (ADR 0008).

## 2. `--dns-search` and the musl ndots trap

`--dns-search cluster.rsrch` writes `search cluster.rsrch` into the
container's resolv.conf, and Docker 29 writes `options ndots:0` on its own
(the generated comment reads `Option ndots from: internal`). musl appends
search domains only to names with fewer dots than ndots, and zero dots isn't
fewer than zero. So alpine sent bare `svc1` upstream as-is, and ping reported
`bad address 'svc1'`. With `--dns-opt ndots:1` the same lookup tried
`svc1.cluster.rsrch` first and resolved to 172.30.0.77.

glibc (the Ubuntu-based Envoy image) retries with the search suffix after the
literal query fails, so it resolved `svc1` even at ndots:0. A glibc smoke
test hides a bug that kills every alpine workload. Aliases mask it too, since
they resolve natively at any ndots, while service names like `b` (the
containers are `b-1`, `b-2`) only exist through the search domain.

k-lite relies on this for: `--dns-search svc.klite --dns-opt ndots:1` on
every workload container, so bare names work on alpine (ADR 0008, ADR 0017).

## 3. Static IPs on a partitioned subnet

`--ip` works only on user-defined networks. On the default bridge the daemon
refuses with `user-specified IP address is supported on user-defined networks
only`. On rsrch0, Docker's IPAM drew dynamic addresses from the bottom of the
`--ip-range` (the first container got 172.30.128.1) while a manual
`--ip 172.30.0.53` outside that range coexisted with it. The CLI reference
documents `--ip-range` as the allocation range, with manual `--ip` allowed
anywhere in the subnet (docs.docker.com/reference/cli/docker/network/create/).
Created without `--gateway`, the bridge itself grabbed 172.30.128.0 out of
the range, which is why klite0 pins its gateway at 10.44.0.1.

    10.44.0.0/16  klite0 subnet
      10.44.0.1        gateway, pinned with --gateway
      10.44.0.x        manual --ip space, klite-net statics
      10.44.64.0/18    VIP pool, allocated by klited, never given to Docker
      10.44.128.0/17   --ip-range, the only space Docker IPAM draws from

k-lite relies on this for: keeping infra statics and the klited-owned VIP
pool clear of instance IPs with no allocator coordination (ADR 0006).

## 4. The VM boundary and the two sanctioned crossings

    macOS host     klited + agents, listeners on 127.0.0.1
      publish   Mac curl 127.0.0.1:18099 ──▶ container :80          works
      gateway   container ──▶ 192.168.5.2 ──▶ Mac 127.0.0.1:18321   works
      direct    Mac ──▶ 10.44.x.x or 172.30.x.x                     no route
    ═══════════════════════════════════════════════ VM boundary (vz)
    colima VM      dockerd + bridges + containers

From macOS, curl to 172.30.128.1:80 failed with exit 7, ping lost every
packet, and the Mac's route table has no entry for 10.44/16 or 172.30/16.
Inbound, `-p 127.0.0.1:18099:80` worked first try, since lima forwards
published ports to the Mac's loopback.

xDS depends on the outbound crossing. `--add-host
host.docker.internal:host-gateway` wrote 192.168.5.2 into /etc/hosts, because
colima starts dockerd with `--host-gateway-ip=192.168.5.2` (visible in the
VM's process list, not in daemon.json). We bound a python http.server to
127.0.0.1 on the Mac and fetched its marker file from inside a container
through `host.docker.internal:18321`. So 192.168.5.2 terminates on the macOS
host with loopback reachable, and nothing in the VM answers on it.

k-lite relies on this for: Envoy dialing klited's xDS listener on the Mac's
loopback, and agents touching containers only via the Docker API and
127.0.0.1-published ports (ADR 0007, ADR 0008).

## 5. Bind mounts ride virtiofs, and only under $HOME

The VM carries exactly one share. `colima ssh -- mount` lists colima's
default vz $HOME mount, `/Users/work_trial type virtiofs (rw,relatime)`, and
nothing else. A `-v` source under $HOME reaches the Mac's
files, and edits propagate live. A running container read v1, the Mac rewrote
the file, and the next read in that same container returned v2. Outside $HOME
the failure is silent, since `-v /opt:/mnt` mounted the VM's /opt (it listed
`containerd`, while the Mac's /opt holds `homebrew`) without any error.

k-lite relies on this for: mounting Envoy bootstrap configs and
`~/.klite/server/tls` certs into infra-pod containers (ADR 0013).

## 6. colima pins

The daemon socket is `unix:///Users/work_trial/.colima/default/docker.sock`
behind the `colima` docker context, and `colima status` reports vz, aarch64,
virtiofs. Inside the VM, /var/lib/docker sits on /dev/vdb1, colima's
persistent data disk, so images, user-defined networks, and stopped
containers are ordinary dockerd state that survives `colima stop` and
`colima start`. State dies only with `colima delete`
(github.com/abiosoft/colima FAQ). Running containers stop with the VM and
come back per Docker restart policy
(docs.docker.com/engine/containers/start-containers-automatically/). We
didn't bounce the VM to prove it while spike agents were live on this daemon.

k-lite relies on this for: knowing what a colima restart keeps (images,
klite0, stopped containers) versus what the agents rebuild (ADR 0011).

## 7. NET_ADMIN makes a /32 VIP real on the bridge

Docker's default capability set drops CAP_NET_ADMIN, so a plain container
can't touch its own interface. `ip addr add 172.30.0.200/32 dev eth0`
answered `RTNETLINK answers: Operation not permitted`. With `--cap-add
NET_ADMIN` the same command stuck, and eth0 carried the /32 next to its
primary 172.30.128.3/16. A peer container then pinged the VIP in
0.127ms, and its /proc/net/arp showed 172.30.0.200 at c2:6a:b3:40:8d:65, the
holder's own eth0 MAC. Linux answers ARP for any local address on the
receiving interface, so bridge peers need no extra routes. capabilities(7)
files interface and address configuration under CAP_NET_ADMIN, and
`--cap-add` grants it per container
(docs.docker.com/engine/containers/run/#runtime-privilege-and-linux-capabilities).

k-lite relies on this for: klite-net holding a VIP per (Service, Node) that
every bridge peer reaches, answered by Envoy's freebind listeners (ADR 0006, 0008).
