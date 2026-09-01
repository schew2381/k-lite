# Real nodes

Every node this cluster has ever run was an agent process on one Mac, all of them sharing colima's Docker daemon (ADR 0003). What actually stands between that and a real Ubuntu box joining from across the internet?

Less than you'd guess, because the design never assumed local. Agents only dial out (ADR 0004), a join needs one token and an outbound connection (ADR 0013), and M9 already routes cross-node traffic to advertised machine addresses behind mTLS (ADR 0034). The gaps were distribution rather than design. M11 built the machinery to close them (ADR 0038): `make release` cross-builds every binary, a tag-triggered workflow publishes releases and a multi-arch `klite-net` image, and `klite node add` plus `hack/join.sh` collapse the join to one pasted line. Two moves remain, and they're the operator's: make the repo (or at least its releases and packages) public, and push the first `v*` tag.

The walkthrough below joins a real box by hand, using nothing beyond the repo itself. It stays the fallback while no public release exists, and it's what join.sh automates. After it comes the gap list with what closed each entry, then the one-command join as it now works.

## Joining a real Ubuntu box today

This assumes the usual dev stack on the Mac (etcd up, klited running, `make build` done) and an Ubuntu box you can ssh into, amd64 in the commands below. The box has to reach the Mac on some address, written `CP_ADDR` here. A LAN IP or a tailnet IP works unchanged, and a port-forwarded public IP needs one extra flag in step 1.

### Step 1: make klited reachable (Mac)

klited binds `127.0.0.1:7443` by default (`cmd/klited/main.go:61`), so restart it listening on everything:

```sh
bin/klited --listen 0.0.0.0:7443
```

Restarting is safe for a live cluster. The CA and admin token persist under `~/.klite/server`, and the serving cert re-mints on every boot anyway (`loadIdentity`, `cmd/klited/main.go:106`). Agents keep their identities and old join tokens stay valid.

That serving cert's SANs cover localhost, the hostname, and every interface address at boot (`sanHosts`, `cmd/klited/main.go:174`), which is why LAN and tailnet addresses just work. An address the Mac doesn't own, like a router-forwarded public IP, has to be added by hand with the flag that already exists (`cmd/klited/main.go:65`):

```sh
bin/klited --listen 0.0.0.0:7443 --tls-san 203.0.113.7
```

Skipping this produces a failure worth recognizing on sight. The join itself succeeds, because the K10 token verifies the server by CA-hash pin instead of hostname (`internal/ca/tls.go:53`). Every dial after that fails, because the agent pins TLS verification to each endpoint's host (`cmd/klite-agent/main.go:119`) and `ca.AgentTLS` does full verification with no skip (`internal/ca/tls.go:28`). The symptom is an agent that minted an identity and then logs `register failed, retrying` with an x509 hostname mismatch forever (`internal/agent/agent.go:169`). The facade met the same class of failure from the other side, and its fix left a comment (`internal/facade/dial.go:118`).

### Step 2: declare the node and mint a token (Mac)

Membership is declared, never discovered (ADR 0018), so the Node object comes first:

```sh
printf 'apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: node-4\nspec:\n  maxInstances: 32\n' \
  | bin/klite apply -f -
bin/klite node token
```

### Step 3: cross-build and copy (Mac)

`make build` compiles for the host alone (`Makefile:18`), darwin/arm64 on this machine. `make release` cross-builds every binary for linux/amd64, linux/arm64, and darwin/arm64 into `dist/` with checksums (`Makefile:26`), so on a current checkout you can skip the hand-rolled lines below and `scp` from `dist/` instead. The recipe is the same either way. Every dependency is pure Go, so a linux build with CGO off comes out statically linked:

```sh
mkdir -p /tmp/wan
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/wan/klite-agent ./cmd/klite-agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/wan/klite-net ./cmd/klite-net
scp /tmp/wan/klite-agent /tmp/wan/klite-net ubuntu@BOX:
```

An arm64 box takes `GOARCH=arm64`.

### Step 4: Docker and the klite-net image (box)

```sh
curl -fsSL https://get.docker.com | sudo sh
```

The agent finds the socket without configuration here, because the probe checks `/var/run/docker.sock` first (`internal/runtime/docker.go:62`) and that's exactly where a stock install puts it. Run the agent as root or put the user in the `docker` group. Rootless Docker is the one layout the probe misses, and `DOCKER_HOST` or `--docker-host` covers it (`cmd/klite-agent/main.go:37`).

The agent's default infra image name is `klite-net:dev` (`internal/agent/infrapod.go:30`), and the daemon-side pull of a bare tag like that resolves against Docker Hub (`infrapod.go:325`, `internal/runtime/docker.go:141`). Docker Hub doesn't have it, so the infra pod never starts. Once a release exists, the fix is one klited flag: `--net-image ghcr.io/schew2381/klite-net:<tag>` hands every node a pullable image through NetBootstrap (ADR 0038). Until then, build the image on the box from the binary you just copied:

```sh
mkdir klite-net-img && mv klite-net klite-net-img/ && cd klite-net-img
printf 'FROM scratch\nCOPY klite-net /klite-net\nENTRYPOINT ["/klite-net"]\n' > Dockerfile
sudo docker build -t klite-net:dev .
cd ..
```

The other two images pull themselves from Docker Hub: `envoyproxy/envoy:v1.31.5` (`infrapod.go:27`) and `alpine:3.20` for the admin-lockdown helper (`internal/agent/lockdown.go:21`). The helper also runs `apk add iptables` at start (`lockdown.go:33`), so the box needs outbound reach to the Alpine mirrors too.

### Step 5: join (box)

```sh
sudo ./klite-agent --node node-4 --server CP_ADDR:7443 \
  --token 'K10<paste from step 2>' \
  --advertise-address "$(curl -4 -s ifconfig.me)"
```

`--advertise-address` is *not* optional on a real node. The default, `host.docker.internal`, is resolved inside the donor, the klite-net container that owns the infra pod's netns. Docker writes its host-gateway address into that container's `/etc/hosts` (`internal/agent/advertise.go:62`), and on Linux the host-gateway is the docker bridge gateway, usually `172.17.0.1`. klited's screen only rejects loopback and unspecified addresses (`internal/server/agent.go:313`), so that value goes into EDS looking legitimate. Every other node's Envoy then dials its own bridge. Advertise the machine's routable address, which on a LAN is the LAN IP rather than whatever `ifconfig.me` answers.

### Step 6: open the ingress slice (box)

Registration assigned the node the smallest free index (`internal/server/agent.go:174`), and the donor's published admin port reveals it, since that port is 19000 plus the index (`internal/agent/infrapod.go:118`):

```sh
sudo docker port klite.node-4.net 9090     # 127.0.0.1:19004 means index 4
```

The node's ingress slice is 32 ports starting at 20000 + 32 × (index − 1) (`internal/controller/ingress.go:24`, `:69`), so index 4 owns 20096 through 20127:

```sh
sudo ufw allow 20096:20127/tcp
```

Those 32 ports are the box's entire inbound surface, since the admin ports publish on loopback alone (`infrapod.go:184`). Each one is an Envoy listener that requires a client certificate chaining to the cluster CA (`internal/xds/builder.go:571`, ADR 0034, ADR 0036), and verify-m9 watches plaintext and foreign-CA dials die in that handshake. Everything else the box does is an outbound dial:

```
  the box after joining (index 4)
  ┌──────────────────────────────────────────────────────────┐
  │ klite-agent ── outbound gRPC/mTLS ──▶ CP_ADDR:7443       │
  │ envoy (xDS) ── outbound gRPC/mTLS ──▶ CP_ADDR:7443       │
  │ dockerd     ── outbound pulls ──────▶ Docker Hub, Alpine │
  │                                                          │
  │ inbound: tcp 20096-20127, envoy's ingress listeners      │
  └──────────────────────────────────────────────────────────┘
```

### What you get

`klite get nodes` shows node-4 Ready, the scheduler spreads new Instances onto it (ADR 0012), and cross-node traffic toward those Instances arrives through the mTLS hop. The reverse leg, box toward Mac, is broken in the way the mixed-clusters section spells out.

Restarts are already boring. The identity under `~/.klite/agent/node-4/tls` outlives the process, so later runs skip the token (`EnsureIdentity`, `internal/agent/join.go:69`). SIGTERM leaves containers running for the next run to adopt (`internal/agent/agent.go:126`). Those two properties are what the one-command join's systemd unit leans on.

For calibration, verify-m8 already rehearsed the join half of this on one machine. Its step 10 (`hack/verify-m8.sh:312`) walks an agent in through the Mac's own LAN IP, proving the token pin and the interface SAN over a non-loopback address. It runs on the same daemon and the same arch though, so it never met the distribution problem. The steps above are exactly the parts it couldn't rehearse.

## The gap list

The table now tracks what M11 closed and what remains.

| Gap                                            | Where                                                                 | Status                                                                                                                       |
| ---------------------------------------------- | --------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `make build` compiles for the host only        | `Makefile:26`                                                          | closed: `make release` cross-builds every binary for linux/amd64, linux/arm64, darwin/arm64, checksums included                |
| no release artifacts, and the repo is private  | `.github/workflows/release.yml`                                        | wiring done: a `v*` tag builds and attaches everything. Making the repo (or its releases) public and pushing the first tag remain the operator's moves |
| `klite-net` image name is a compile-time const | `internal/agent/infrapod.go:30`                                        | closed: `net_image` rides NetBootstrap, set by klited's `--net-image`, and empty keeps `klite-net:dev` (ADR 0038)              |
| `klite-net` image is arm64-only, never pushed  | `build/klite-net.Dockerfile`, `.github/workflows/release.yml`          | closed: TARGETARCH-parameterized COPY, and the release pushes a multi-arch manifest to `ghcr.io/schew2381/klite-net:<tag>`     |
| socket probe misses rootless Docker            | `internal/runtime/docker.go:62`                                        | open by choice: rootless hosts set `DOCKER_HOST` by hand, stock Ubuntu already works                                            |
| addresses the server doesn't own miss the SANs | `--tls-san` exists, `cmd/klited/main.go:65`                            | still documentation: the flag exists, step 1 shows it                                                                           |
| klited binds loopback                          | `--listen` exists, `cmd/klited/main.go:61`                             | still documentation: the flag exists, step 1 shows it                                                                           |
| default advertise address poisons Linux nodes  | `cmd/klite-agent/main.go:38`, screen at `internal/server/agent.go:313` | contained: join.sh always passes `--advertise-address` and refuses to guess from private detections, and `node add` prints the warning. The agent's own default is unchanged, so hand-run agents still need the flag |
| joining is six manual steps                    | `internal/cli/node.go:35`, `hack/join.sh`                              | closed: `klite node add` prints the line, join.sh does the rest                                                                 |

The private-repo row is still the one with a decision inside it, now the only one. Release assets on a private repo demand a token to download, so the curl-pipe join needs the repo public, or at least its releases (and its packages, for the ghcr image). Until that flip, the walkthrough above is the working path.

## The one-command join

The shape is k3s's, a curl-piped script driven by environment variables, with the server side printing the exact line to paste.

### Server side: klite node add

`klite node add <name>` (`internal/cli/node.go:35`) composes what steps 1 and 2 do by hand: it applies the Node object (`--labels` and `--max-instances` as flags), calls the existing NodeToken RPC, and prints the paste-ready line plus the copy-the-binary fallback for machines the releases don't cover.

```
$ klite node add node-4 --url 203.0.113.7:7443
node/node-4 created

join from the new machine (Linux with systemd, as root):

  curl -sfL https://github.com/schew2381/k-lite/releases/latest/download/join.sh | \
    KLITE_URL=203.0.113.7:7443 KLITE_TOKEN='K10…' KLITE_NODE=node-4 sh -
```

klited doesn't know which address agents should dial (`sanHosts` collects every interface, not the chosen one), which is why the `--url` flag exists. It defaults to the CLI's own server endpoint, and a loopback value makes the printout flag it and ask for one the new machine can reach.

### Node side: join.sh

The script (`hack/join.sh`) is five ordered moves.

1. Check for Docker. When it's missing, install via get.docker.com only under an explicit `KLITE_YES=1`. Otherwise print that exact command and exit.
2. Pick the release binary by `uname -m` (`KLITE_VERSION` pins a tag, latest otherwise), drop it at `/usr/local/bin/klite-agent`. A failed download names the likely cause: the repo or its releases still private, or no release published yet.
3. Default `KLITE_ADVERTISE` to the detected public IPv4, and refuse to proceed when detection only finds a private or Docker-bridge address. The agent's own default is wrong on Linux (step 5 above), and a silently-advertised `172.17.0.1` poisons every node that dials it.
4. Write `/etc/klite/agent.env` (0600, the token lives there) and the unit below.
5. `systemctl enable` and start `klite-agent`, then print the watch to run from the control plane: `klite get nodes -w`.

```ini
[Unit]
Description=k-lite node agent
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service

[Service]
EnvironmentFile=/etc/klite/agent.env
ExecStart=/usr/local/bin/klite-agent \
  --node ${KLITE_NODE} \
  --server ${KLITE_URL} \
  --token ${KLITE_TOKEN} \
  --advertise-address ${KLITE_ADVERTISE} \
  --state-dir /var/lib/klite
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

The walkthrough already proved restarts boring, which is what makes `Restart=always` safe. The firewall step stays manual, or moves into the script once it can read back the index klited assigns at registration.

### The wiring behind it

- A tag-triggered workflow (`.github/workflows/release.yml`) runs `make release` and attaches all four binaries for linux amd64, linux arm64, and darwin arm64, the checksums file, and `join.sh` itself to a GitHub Release. The cross-build recipe is the proven one, the `make net-image` line with different GOOS/GOARCH pairs.
- The same workflow pushes `ghcr.io/schew2381/klite-net:<tag>` as a multi-arch manifest built with `docker buildx`. The Dockerfile's once-hardcoded arm64 binary path is a `TARGETARCH`-parameterized COPY now, so dev and released images share one recipe.
- The image reference moved from const to wire (ADR 0038). `NetBootstrap` already carries the cluster-level knobs every node must agree on (port bases, infra IPs, cluster ID). The `net_image` field beside them (`api/proto/klite/v1/agent.proto`), set by klited's `--net-image`, pins the image cluster-wide, and the fallback to `klite-net:dev` in `infrapod.go` keeps dev unchanged when the field is empty. A per-agent flag would work too, but image-version skew across nodes is exactly the kind of drift NetBootstrap exists to prevent. The image change rides the donor's config hash, so a flip recreates each infra pod exactly once.
- The release also carries the built web board as `klite-ui-<tag>.tar.gz`. Extract it anywhere and `klite-facade --ui-dir <dir>` serves it, no dev server involved.

### The alternatives

| Approach                                   | Works today | Node needs                              | Where it hurts                                                                 |
| ------------------------------------------ | ----------- | --------------------------------------- | ------------------------------------------------------------------------------ |
| join.sh plus releases plus ghcr (built)    | once the repo is public and a tag exists | curl and root | the release wiring is this repo's to maintain, and the repo (or its releases and packages) goes public |
| clone the repo, `go build` on the node     | yes         | git access to the private repo, Go 1.27 | a toolchain install per node, and the node holds repo credentials               |
| agent shipped as a container               | no          | a registry the node can pull from       | mounts the docker socket and state dir to manage the host from inside a container, an extra layer over one static binary, and it has the same registry gap as klite-net |
| config management (ansible and friends)    | yes         | ssh from a control machine              | the right shape at fleet scale, three files of ceremony for one box             |

Clone-and-build stays the fallback that works this afternoon against the private repo, and it's fine for the first box or two. The script plus releases got the effort (ADR 0038), because it's also what makes "wipe the box and re-join" a single line.

## Mixed clusters are one-way

The Mac's nodes advertise what the default flag resolves to, colima's host-gateway address `192.168.5.2` (`research/docker-networking-macos.md:84`, `internal/agent/advertise.go:62`). That address exists inside the lima VM and nowhere else. EDS hands it to every consumer verbatim (`internal/controller/endpoints.go:392`, `internal/xds/builder.go:479`), and nothing between the flag and the wire knows it's private.

```
  real box B (advertises 203.0.113.9)          mac node A (advertises 192.168.5.2)

  envoy B ◀════ mTLS to 203.0.113.9:20096 ════ envoy A        works
  envoy B ════ dial 192.168.5.2:20003 ═══▶ x                  unroutable
```

So traffic flows toward real nodes and dies toward local ones. A request on the box that picks a Mac endpoint fails outright rather than getting rerouted, because the endpoint looks dialable in EDS and there are no active health checks to eject it. `research/overlay-wan.md:39` predicted this exact trap for NATed nodes, the node that "joins the cluster, heartbeats status, and looks healthy" while "every cross-node request aimed at it then blackholes, because joining never proved anyone could dial in."

The rules to plan around:

- An all-real-nodes cluster is clean. Every node advertises a routable address, so every leg works both ways.
- A mixed cluster is one-way. Real nodes serve, Mac nodes consume, and a Workload with endpoints on the Mac is partially broken for every consumer on a real node, roughly in proportion to the Mac's share of its endpoints.

Nothing in the scheduler steers around this, and ADR 0034 named mutual reachability as its precondition out loud. When NATed or mixed topologies need to actually work, the shipped answer is an overlay network underneath the unchanged published-port dialer (the "Over the open internet" section below, ADR 0043). The WireGuard mesh (`research/overlay-wan.md`, option B), which would replace the dialer instead of running under it, stays the recorded native future.

## What deliberately doesn't change

ADR 0016 deferred cross-machine traffic by naming the only two seams allowed to move, the cross-node endpoint dialer and IPAM. ADR 0034 then moved the dialer. Going from local agent processes to real machines moves neither, and every interface stays where it is.

- The Runtime interface keeps driving whatever dockerd the socket reaches, and a real box's daemon speaks the same API as colima's (`internal/runtime/runtime.go`, ADR 0019).
- etcd stays wherever klited lives, invisible to agents (ADR 0005).
- Dial-out gRPC was chosen so that "a node behind NAT joins a WAN cluster with no design change" (ADR 0004), and that sentence is now load-bearing.
- The token-to-certificate join and declared Node objects never assumed a machine shape, which verify-m8 has exercised over a non-loopback address since M8 (ADR 0013, ADR 0018).
- The data-plane hop was designed and verified assuming the two Envoys share nothing but the cluster CA (ADR 0034 through 0036).

The one wire change this doc proposed, now landed, is the additive `net_image` field on NetBootstrap (ADR 0038), and old agents that don't read it keep their compiled-in default. Everything else lives in the Makefile, the release workflow, one const, one script, and one CLI command.

## Over the open internet

Everything above assumed some address exists that both machines can dial. Across the open internet, with NAT (or carrier-grade NAT) on both ends, no such address exists by default. The instinctive fix (forward some ports) is worth pricing out before reaching for it.

The control plane needs one inbound address: agents and Envoys only dial out to klited (ADR 0004), so one router forward of tcp/7443 covers every node that will ever join. The data plane is the multiplier. ADR 0034 requires every node to be dialable at `advertise:ingress-slice` by every other node, so each node's network needs its own router access, its own 32-port forward, and a stable public IP or dynamic DNS. A machine on LTE, or behind the CGNAT many ISPs now default to, has no router to forward ports on. Its "public" IP is shared, and inbound is simply not on offer. Port maps scale per-network and die entirely on CGNAT. An overlay gives every machine one address that works from anywhere the machine can dial out, and it costs one command per machine.

Three patterns follow, best first, and all of them leave k-lite's code alone. The join flow, the advertise path, and the mTLS ingress hop run unchanged because nothing in the chain treats an overlay IP specially:

- The agent accepts any literal non-loopback IP (`internal/agent/advertise.go:23`).
- klited's screen rejects only loopback and unspecified (`internal/server/agent.go:393`).
- EDS renders whatever was advertised (`internal/xds/builder.go:479`).
- The donor publishes the ingress slice on `0.0.0.0` (`internal/agent/infrapod.go:202`), so inbound on an overlay interface lands on the same listeners.

### Pattern 1: a tailnet (recommended)

Tailscale gives every enrolled machine a stable IP in `100.64.0.0/10`, coordinates WireGuard tunnels between them, punches through NAT where possible, and relays through its DERP servers where not. Both ends only ever dial out, which is why it works from LTE, hotel Wi-Fi, and CGNAT, the exact places port forwarding can't go. The cost is a third-party account (the free plan carries unlimited devices under your own user, which is all a cluster consumes) and their coordination servers in the loop.

On the Mac, install Tailscale (the app from tailscale.com, or `brew install tailscale` for the headless daemon), sign in, and restart klited so this boot's serving cert picks up the new interface. The restart matters because `sanHosts` collects interface addresses at mint time (`cmd/klited/main.go:176`): a klited started after tailscaled needs no flags, and one started before it needs a restart or an explicit `--tls-san`.

```sh
bin/klited --listen 0.0.0.0:7443
tailscale ip -4        # say it prints 100.101.102.103
klite node add lte-box --url 100.101.102.103:7443
```

`node add` notices the tailnet URL and prints the join line in `join.sh`'s tailscale mode:

```sh
curl -sfL https://github.com/schew2381/k-lite/releases/latest/download/join.sh | \
  KLITE_URL=100.101.102.103:7443 KLITE_TOKEN='K10…' KLITE_NODE=lte-box \
  KLITE_VPN=tailscale KLITE_TS_AUTHKEY='tskey-auth-…' KLITE_YES=1 sh -
```

On the new box, that line installs tailscale (official script, consented via `KLITE_YES=1`), joins the tailnet with the auth key, derives the advertise address from `tailscale ip -4`, and proceeds as any other join. Mint the key at the admin console's Keys page. One-off keys are the right choice. Tailscale expires node keys (180 days by default), and an expired node drops off the tailnet looking exactly like a network failure. Disable key expiry for cluster machines while you're in the console.

Advertise tailnet IPs everywhere, including on the Mac's own local agents when the cluster mixes local and remote nodes. Local agents advertising colima's host-gateway (or even the LAN IP) leave Mac-hosted endpoints unreachable from across the internet, the one-way trap the mixed-clusters section describes. The same `--advertise-address` lever the LAN playground uses (`hack/dev-up.sh:42`), pointed at the Mac's tailnet IP, closes it. Machines that share a LAN lose nothing: Tailscale spots LAN endpoints and keeps same-network peers on the direct local path (`wgengine/magicsock`, endpoints of type local), so their traffic never detours through the internet. That is what makes "put everything on the tailnet and advertise only tailnet IPs" a universal rule rather than a WAN-only one.

Stick to the raw `100.x` IPs in `--url`, `--server`, and `KLITE_ADVERTISE`. MagicDNS names mostly resolve, since the agent falls back to host DNS (`internal/agent/advertise.go:75`), which tailscaled manages on both platforms. But EDS itself carries only literal IPs, klited's cert doesn't include the MagicDNS name unless `--tls-san` adds it, and a name that resolves on one machine and not another is a debugging session. The IPs are stable for the machine's tailnet life.

k3s ships this same integration shape as `--vpn-auth="name=tailscale,joinKey=…"`: it starts tailscale, reads the IP from `tailscale status --json`, and uses it as the node's address (`pkg/vpn/vpn.go` in the k3s tree). The parts k3s deliberately leaves to the operator (the account, the key, running etcd traffic over the tunnel) this repo leaves out too.

### Pattern 2: single-hub WireGuard (no account, one forwardable router)

When a third-party coordinator is unacceptable, plain WireGuard in a hub-and-spoke shape needs exactly one network that can forward a port: the hub's. Every spoke dials out to the hub and keeps its NAT mapping alive with keepalives, so spokes can live behind CGNAT. Spoke-to-spoke traffic relays through the hub, which is the topology's honest cost next to a tailnet's peer-to-peer tunnels.

The hub is the Mac (it's already the one address every node dials). Install and generate keys:

```sh
brew install wireguard-tools
wg genkey | tee hub.key | wg pubkey > hub.pub        # once for the hub
wg genkey | tee box1.key | wg pubkey > box1.pub      # once per spoke
```

Hub config, `/opt/homebrew/etc/wireguard/wg0.conf` (one `[Peer]` block per spoke):

```ini
[Interface]
PrivateKey = <contents of hub.key>
Address = 10.99.0.1/24
ListenPort = 51820

[Peer]
PublicKey = <contents of box1.pub>
AllowedIPs = 10.99.0.2/32
```

Spoke config, `/etc/wireguard/wg0.conf` on box-1 (Linux: `apt install wireguard`):

```ini
[Interface]
PrivateKey = <contents of box1.key>
Address = 10.99.0.2/24

[Peer]
PublicKey = <contents of hub.pub>
Endpoint = <hub public IP or DDNS name>:51820
AllowedIPs = 10.99.0.0/24
PersistentKeepalive = 25
```

Then forward udp/51820 to the Mac on the router and bring the interface up on both ends:

```sh
sudo sysctl -w net.inet.ip.forwarding=1   # Mac: relay spoke-to-spoke traffic
sudo wg-quick up wg0                      # Mac
sudo systemctl enable --now wg-quick@wg0  # spokes
```

Join exactly as the walkthrough above, with the wg addresses in both seats: klited reachable at `10.99.0.1:7443` (restart it after `wg-quick up` so the cert covers the wg address, or `--tls-san 10.99.0.1`), and each spoke joining with `KLITE_URL=10.99.0.1:7443 KLITE_ADVERTISE=10.99.0.<n>`. The 25-second keepalive is what holds each spoke's NAT mapping open so the hub's packets keep arriving. If the Mac's own network is CGNAT, the hub moves to any cheap VPS and the Mac becomes one more spoke, with the same configs and one more peer block.

The recipe doesn't give you key distribution (you carried the pubkeys by hand), revocation (delete the peer block everywhere), or a second path when the hub is down. That's the operational surface Tailscale automates, bought here for zero accounts and one UDP port.

### Pattern 3: the raw paths

#### Port-forward everything

Forward tcp/7443 to the control plane and each node's 32-port ingress slice on its own router, run klited with `--tls-san <public-ip>` (the cert can't know a router's address), and set `KLITE_ADVERTISE` to each node's public IP. It works when every machine sits behind a router you control on a stable address, and not at all behind CGNAT. This is the walkthrough's "port-forwarded public IP" path, and its price is per-network router work that the overlay patterns replace with one command.

#### IPv6 end to end

With global IPv6 on every machine and firewalls opened for the ingress slices, there's no NAT to traverse at all. The chain is v6-clean on paper: the agent takes any literal IP (`internal/agent/advertise.go:23`) and klited's SAN collection includes v6 interface addresses. But no cluster has run this way yet, `join.sh`'s detection is IPv4-only (set `KLITE_ADVERTISE` by hand), and residential v6 firewalls and flapping prefixes are their own project. Recorded as plausible, not supported.

#### Reverse tunnels (the native future)

Tools like rathole and frp invert the direction: the NATed machine dials out to a relay on a public VPS, which exposes its ports. That shape fits k-lite unusually well because klited is already the one address every node dials, the lighthouse seat `research/overlay-wan.md` assigned it when the mesh was still option B. A native version (agents keep a tunnel open, and klited or a sibling relays ingress traffic) would drop the third-party dependency and the per-machine VPN. The price is building and operating a relay in the data path, the thing ADR 0034 explicitly refused ("the control plane must never sit in the data path"). It stays recorded as the future option, not started (ADR 0043).

### The LTE-hotspot test

The cleanest true-internet test needs no second household: a Linux laptop tethered to a phone hotspot is a different network, behind CGNAT, with zero router access. If the join works there, it works anywhere. From the cluster's Mac:

1. Install Tailscale, sign in, and note `tailscale ip -4` (call it `100.101.102.103`).
2. Restart klited listening wide, so the fresh cert bakes the tailnet address in: `bin/klited --listen 0.0.0.0:7443`. Skipping the restart earns the walkthrough's signature failure. The join succeeds, and the agent then loops on `register failed, retrying` with an x509 hostname mismatch forever.
3. Mint a tailnet auth key (admin console → Settings → Keys, one-off is fine) and declare the node: `klite node add lte-box --url 100.101.102.103:7443`. Copy the printed tailscale-mode line.
4. On the laptop: turn Wi-Fi off, tether to the phone, confirm it's really on carrier internet (`curl ifconfig.me` shows an address you don't recognize). Paste the join line with the real auth key in place of the placeholder.
5. Watch from the Mac: `klite get nodes -w` until lte-box is Ready. Then make traffic cross: scale a Workload until Instances land on lte-box (`klite get instances` shows the node), and drive requests at its Service from the Mac side (the board's traffic feed or the demo probers both work). Every request that lands proves the full path: EDS handed the Mac's Envoys `100.x:ingress-port`, the mTLS handshake crossed the tunnel, and the pod answered.
6. `tailscale status` on either machine names the path. A direct address means NAT traversal won, and `relay "…"` means DERP is carrying it. Both count: relay is the guarantee that makes the recommendation safe, not a degraded mode to apologize for.

The reverse leg (laptop-hosted consumers dialing Mac-hosted endpoints) stays broken until the Mac's local agents advertise the tailnet IP, per the mixed-cluster rule above. For the demo's shape (Mac as control plane and consumer, remote box as capacity), the one-way cluster is already the whole show.
