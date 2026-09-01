# Real nodes

Every node this cluster has ever run was an agent process on one Mac, all of them sharing colima's Docker daemon (ADR 0003). What actually stands between that and a real Ubuntu box joining from across the internet?

Less than you'd guess, because the design never assumed local. Agents only dial out (ADR 0004), a join needs one token and an outbound connection (ADR 0013), and M9 already routes cross-node traffic to advertised machine addresses behind mTLS (ADR 0034). The gaps are distribution rather than design. Nothing builds linux binaries, the `klite-net` image exists only inside daemons where `make net-image` ran, and joining takes several manual steps per node.

The walkthrough below joins a real box using only what exists today. After it comes the gap list, each entry with the file it lives in, then the one-command join worth building instead.

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

`make build` compiles for the host alone (`Makefile:9`), darwin/arm64 on this machine. Every dependency is pure Go, so a linux build with CGO off comes out statically linked, the same recipe `make net-image` already uses for arm64 (`Makefile:15`):

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

The agent hardcodes the infra image name `klite-net:dev` (`internal/agent/infrapod.go:26`) and asks the daemon to pull it when missing (`infrapod.go:310`, `internal/runtime/docker.go:141`). No registry has it, so the pull fails and the infra pod never starts. Build it on the box from the binary you just copied:

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

An S is under an hour of work and an M is a day-ish. The "none" rows cost nothing but knowing the flag exists.

| Gap                                            | Where                                                                | Effort | Without it                                                                    |
| ---------------------------------------------- | -------------------------------------------------------------------- | ------ | ----------------------------------------------------------------------------- |
| `make build` compiles for the host only        | `Makefile:9`                                                          | S      | every node needs step 3's hand-rolled cross-build                              |
| no release artifacts, and the repo is private  | `.github/workflows/ci.yml` (build and test only)                      | M      | a join script has nothing to curl, and `go install` wants GOPRIVATE plus creds |
| `klite-net` image name is a compile-time const | `internal/agent/infrapod.go:26`                                       | S      | agents only run against daemons someone hand-loaded the image into             |
| `klite-net` image is arm64-only, never pushed  | `build/klite-net.Dockerfile:4`, `Makefile:14`                         | M      | amd64 boxes need step 4's hand-written Dockerfile                              |
| socket probe misses rootless Docker            | `internal/runtime/docker.go:62`                                       | S      | rootless hosts set `DOCKER_HOST` by hand (stock Ubuntu already works)          |
| addresses the server doesn't own miss the SANs | `--tls-san` exists, `cmd/klited/main.go:65`                           | none   | join succeeds, then Register loops forever on an x509 mismatch                 |
| klited binds loopback                          | `--listen` exists, `cmd/klited/main.go:61`                            | none   | remote agents get connection refused                                           |
| default advertise address poisons Linux nodes  | `cmd/klite-agent/main.go:38`, screen at `internal/server/agent.go:313` | S      | cross-node dials land on the dialer's own docker bridge                        |
| joining is six manual steps                    | `internal/cli/node.go` has only `node token`                          | M      | steps 1 through 6, per node, forever                                           |

The private-repo row is the one with a decision inside it. Release assets on a private repo still demand a token to download, so the curl-pipe join below needs the repo public, or at least its releases. Until then the works-today path is cloning and building on the node, weighed against the other approaches in the alternatives table.

Everything else is mechanical. The two "none" rows cost documentation and the S rows are an afternoon combined, which leaves the release wiring, the registry, and the join script as the real work.

## The one-command join

The shape worth copying is k3s's, a curl-piped script driven by environment variables, with the server side printing the exact line to paste.

### Server side: klite node add

`klite node` has one subcommand today, `token` (`internal/cli/node.go:21`). A `node add <name>` would compose what steps 1 and 2 do by hand, applying the Node object, calling the existing NodeToken RPC, and printing the paste-ready line.

```
$ klite node add node-4
node/node-4 created
join from the new machine with:

  curl -sfL https://github.com/schew2381/k-lite/releases/latest/download/join.sh | \
    KLITE_URL=203.0.113.7:7443 KLITE_TOKEN='K10…' KLITE_NODE=node-4 sh -
```

One wrinkle: klited doesn't know which address agents should dial (`sanHosts` collects every interface, not the chosen one), so `node add` needs a `--url` flag or a config default for the printed line.

### Node side: join.sh

The script is five ordered moves.

1. Install Docker when `docker info` fails, via get.docker.com.
2. Pick the release binary by `uname -s` and `uname -m`, drop it at `/usr/local/bin/klite-agent`.
3. Default `KLITE_ADVERTISE` to `curl -4 -s ifconfig.me` when unset, since the agent's own default is wrong on Linux (step 5 above).
4. Write `/etc/klite/agent.env` (0600, the token lives there) and the unit below.
5. `systemctl enable --now klite-agent`.

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

- A tag-triggered release job lands beside the existing CI job (`.github/workflows/ci.yml`), attaching `klite`, `klited`, and `klite-agent` for linux amd64, linux arm64, and darwin arm64, plus `join.sh` itself. The cross-build recipe is proven, it's the `make net-image` line with different GOOS/GOARCH pairs.
- The image goes to `ghcr.io/schew2381/klite-net` as a multi-arch manifest built with `docker buildx`. The Dockerfile's hardcoded arm64 binary path (`build/klite-net.Dockerfile:4`) becomes a `TARGETARCH`-parameterized COPY.
- The image reference moves from const to wire. `NetBootstrap` already carries the cluster-level knobs every node must agree on (port bases, infra IPs, cluster ID). A `net_image` field beside them (`api/proto/klite/v1/agent.proto`), set by a klited flag, lets klited pin the image cluster-wide, and a fallback to `klite-net:dev` in `infrapod.go` keeps dev unchanged when the field is empty. A per-agent flag would work too, but image-version skew across nodes is exactly the kind of drift NetBootstrap exists to prevent.

### The alternatives

| Approach                                   | Works today | Node needs                              | Where it hurts                                                                 |
| ------------------------------------------ | ----------- | --------------------------------------- | ------------------------------------------------------------------------------ |
| join.sh plus releases plus ghcr (recommended) | no          | curl and root                           | someone builds and maintains the release wiring, and the repo (or its releases) goes public |
| clone the repo, `go build` on the node     | yes         | git access to the private repo, Go 1.27 | a toolchain install per node, and the node holds repo credentials               |
| agent shipped as a container               | no          | a registry the node can pull from       | mounts the docker socket and state dir to manage the host from inside a container, an extra layer over one static binary, and it has the same registry gap as klite-net |
| config management (ansible and friends)    | yes         | ssh from a control machine              | the right shape at fleet scale, three files of ceremony for one box             |

Clone-and-build is the fallback that works this afternoon against the private repo, and it's fine for the first box or two. The script plus releases is where the effort should go, because it's also what makes "wipe the box and re-join" a single line.

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

Nothing in the scheduler steers around this, and ADR 0034 named mutual reachability as its precondition out loud. When NATed or mixed topologies need to actually work, the recorded next step is the WireGuard mesh (`research/overlay-wan.md`, option B), which replaces the published-port dialer instead of stacking on it.

## What deliberately doesn't change

ADR 0016 deferred cross-machine traffic by naming the only two seams allowed to move, the cross-node endpoint dialer and IPAM. ADR 0034 then moved the dialer. Going from local agent processes to real machines moves neither, and every interface stays where it is.

- The Runtime interface keeps driving whatever dockerd the socket reaches, and a real box's daemon speaks the same API as colima's (`internal/runtime/runtime.go`, ADR 0019).
- etcd stays wherever klited lives, invisible to agents (ADR 0005).
- Dial-out gRPC was chosen so that "a node behind NAT joins a WAN cluster with no design change" (ADR 0004), and that sentence is now load-bearing.
- The token-to-certificate join and declared Node objects never assumed a machine shape, which verify-m8 has exercised over a non-loopback address since M8 (ADR 0013, ADR 0018).
- The data-plane hop was designed and verified assuming the two Envoys share nothing but the cluster CA (ADR 0034 through 0036).

The one wire change this doc proposes is the additive `net_image` field on NetBootstrap, and old agents that don't read it keep their compiled-in default. Everything else lives in the Makefile, the CI file, one const, and one CLI command.
