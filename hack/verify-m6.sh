#!/usr/bin/env bash
# Checks M6 end to end: NetworkPolicy enforcement in the data plane (source-node
# Envoy RBAC, ADR 0009) agreeing with the control plane (`klite policy check`)
# across every policy shape — baseline allow, DENY, ALLOW-flip to allowlist
# mode, DENY with an except list, and full removal. DNS keeps resolving denied
# destinations (ADR 0017). Each transition gates on BOTH planes: log evidence
# from client workloads and the PolicyCheck verdict must never disagree.
#
# This stack, and only this stack, is what the script creates and destroys:
#   klited            127.0.0.1:6443 (cluster token m6-token)
#   etcd members      etcd-m6-1..3 on 127.0.0.1:5379/5381/5383, network klite-etcd-m6
#   nodes             m6-1, m6-2 (agent processes, pidfiles under /tmp/klite-m6)
#   workloads         a, d (alpine wget loops), b, c (traefik/whoami x2)
#   binaries          /tmp/m6-bin (never touches the shared bin/)
# Shared fixtures (klite0, images, other stacks' etcd) are left in place.
#
# Isolation on the shared Docker daemon needs two seams beyond names and ports,
# because other stacks (canonical node-1..3, m7-1..3) share the klite0 bridge:
#
#   1. Node indexes. Register hands out the smallest free index per cluster,
#      so a fresh etcd repeats 1, 2, ..., which are the same donor IPs
#      (10.44.0.11+) and host admin ports (19001+/19501+) other stacks hold.
#      A colliding agent EVICTS the other stack's donor (evictNetSquatters
#      matches on IP).
#      Register keeps a pre-set status.nodeIndex, so this script picks free
#      indexes (8+) and writes them into the node objects in etcd before any
#      agent registers.
#
#   2. VIPs. The allocator hands out first-free from 10.44.64.1 per cluster,
#      and every donor binds its VIPs on the shared bridge, so two clusters
#      answering ARP for the same address cross-wire their data planes. The
#      allocator never rewrites an existing allocation, so after the services
#      land this script rewrites its allocations to a distant slice
#      (10.44.96.1-8) before any agent starts binding.
#
# Exits nonzero on the first gating failure. KEEP_M6=1 skips teardown on exit.
set -u

cd "$(dirname "$0")/.."
REPO="$(pwd)"
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

WORK=/tmp/klite-m6
BIN=/tmp/m6-bin
KLITE="$BIN/klite"
EP=127.0.0.1:6443
export KLITE_SERVER="$EP"
ETCD_EPS="127.0.0.1:5379,127.0.0.1:5381,127.0.0.1:5383"
TOKEN=m6-token
NODES="m6-1 m6-2"
VIP_SLICE_PREFIX=10.44.96 # rewrite target inside 10.44.64.0/18, far from first-free
KLITED_PID=""
STEP=prep

pass() { echo "PASS [$STEP]: $1"; }
info() { echo "INFO [$STEP]: $1"; }
die()  { echo "FAIL [$STEP]: $1"; echo "logs under $WORK"; exit 1; }

# wait_for <seconds> <fn> [args...]: retries every 0.5s within the budget.
wait_for() {
  local budget=$1; shift
  local tries=$((budget * 2))
  for _ in $(seq 1 "$tries"); do
    "$@" && return 0
    sleep 0.5
  done
  return 1
}

# ---------- teardown: strictly m6-scoped, idempotent ----------
teardown() {
  local f n ids
  for f in "$WORK"/agent-m6-*.pid "$WORK"/klited.pid; do
    [ -f "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null
  done
  pkill -9 -f "$BIN/" 2>/dev/null # backstop: only processes from the m6 bin dir
  for n in $NODES; do
    ids=$(docker ps -aq --filter "label=io.klite.node=$n")
    [ -n "$ids" ] && echo "$ids" | xargs docker rm -f >/dev/null 2>&1
  done
  # Containers are named klite.<node>.<x>, so this catches m6 leftovers that
  # predate labels without touching node-* or m7-*.
  ids=$(docker ps -aq --filter "name=klite.m6-")
  [ -n "$ids" ] && echo "$ids" | xargs docker rm -f >/dev/null 2>&1
  ETCD_NAME_PREFIX=etcd-m6 ETCD_PORT_BASE=5379 ETCD_NET=klite-etcd-m6 \
    hack/etcd-up.sh down >/dev/null 2>&1
  rm -rf "$HOME/.klite/etcd/etcd-m6-1" "$HOME/.klite/etcd/etcd-m6-2" \
         "$HOME/.klite/etcd/etcd-m6-3" "$HOME/.klite/agent/m6-1" \
         "$HOME/.klite/agent/m6-2" 2>/dev/null
  return 0
}
on_exit() {
  if [ "${KEEP_M6:-0}" = 1 ]; then
    echo "KEEP_M6=1: leaving the m6 stack running"
  else
    teardown
  fi
}
trap on_exit EXIT

# ---------- etcd surgery helpers (the two isolation seams) ----------
etcd_get() { docker exec etcd-m6-1 etcdctl get "$1" --print-value-only 2>/dev/null; }
etcd_put() { docker exec etcd-m6-1 etcdctl put "$1" "$2" >/dev/null 2>&1; }

# inject_node_index <node> <idx>: set status.nodeIndex on the stored node
# object so Register keeps it instead of assigning first-free. This is a plain
# read-modify-write, and the verify-and-retry loop absorbs a racing controller
# CAS.
inject_node_index() {
  local node=$1 idx=$2 key="/klite/v1/nodes/$1" val new back
  for _ in 1 2 3 4 5; do
    val=$(etcd_get "$key")
    [ -n "$val" ] || { sleep 0.5; continue; }
    new=$(python3 -c '
import json, sys
o = json.loads(sys.argv[1])
o["node"].setdefault("status", {})["nodeIndex"] = int(sys.argv[2])
print(json.dumps(o))' "$val" "$idx") || return 1
    etcd_put "$key" "$new" || return 1
    sleep 0.5
    back=$(etcd_get "$key")
    if node_index_is "$node" "$idx" && [ -n "$back" ]; then
      return 0
    fi
  done
  return 1
}

node_index_is() { # <node> <idx>: stored status.nodeIndex equals idx
  etcd_get "/klite/v1/nodes/$1" | python3 -c '
import json, sys
o = json.load(sys.stdin)
sys.exit(0 if o["node"].get("status", {}).get("nodeIndex") == int(sys.argv[1]) else 1)' "$2" 2>/dev/null
}

rewrite_vip() { # <service> <node> <ip>: repoint one allocation's spec.vip
  local key="/klite/v1/vipallocations/$1.$2" val new
  val=$(etcd_get "$key")
  [ -n "$val" ] || return 1
  new=$(python3 -c '
import json, sys
o = json.loads(sys.argv[1])
o["vipAllocation"]["spec"]["vip"] = sys.argv[2]
print(json.dumps(o))' "$val" "$3") || return 1
  etcd_put "$key" "$new"
}

vip_is() { # <service> <node> <ip>
  etcd_get "/klite/v1/vipallocations/$1.$2" | python3 -c '
import json, sys
o = json.load(sys.stdin)
sys.exit(0 if o["vipAllocation"]["spec"].get("vip") == sys.argv[1] else 1)' "$3" 2>/dev/null
}

alloc_exists() { [ -n "$(etcd_get "/klite/v1/vipallocations/$1.$2")" ]; }
all_allocs_exist() {
  local s n
  for s in a b c d; do
    for n in $NODES; do alloc_exists "$s" "$n" || return 1; done
  done
}

# ---------- cluster read helpers (always through the CLI) ----------
klited_serves()  { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()    { [ "$("$KLITE" get nodes 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')" = 2 ]; }
ready_count()    { "$KLITE" get instances 2>/dev/null | awk -v wl="$1" '$2==wl && $4=="Ready"' | wc -l | tr -d ' '; }
abc_ready()      { [ "$(ready_count a)" = 1 ] && [ "$(ready_count b)" = 2 ] && [ "$(ready_count c)" = 2 ]; }
d_ready()        { [ "$(ready_count d)" = 1 ]; }
infra_up() {
  local n
  for n in $NODES; do
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.envoy" || return 1
  done
}

# ---------- both-planes policy helpers ----------
# Client loops print one line per second: "HH:MM:SS b => <res> | c => <res>",
# where <res> is "Hostname: ..." on success and "<t> FAILED" on failure.

client_inst() { # a|d -> its instance name
  case "$1" in
    a) echo "$A_INST" ;;
    d) echo "$D_INST" ;;
  esac
}

seg_ok() { # <line> <target> <allowed|denied>
  local seg
  seg=$(printf '%s\n' "$1" | tr '|' '\n' | grep "$2 =>")
  case "$3" in
    allowed) printf '%s' "$seg" | grep -q 'Hostname:' ;;
    denied)  printf '%s' "$seg" | grep -q 'FAILED' ;;
  esac
}

# pair_is <from> <to> <allowed|denied>: the client's last two log lines both
# show the wanted state (one-second loop period, so this spans ~2s of traffic).
pair_is() {
  local inst lines n line
  inst=$(client_inst "$1")
  lines=$("$KLITE" logs "$inst" --tail 2 2>/dev/null | grep "$2 =>") || return 1
  n=$(printf '%s\n' "$lines" | grep -c .)
  [ "$n" -ge 2 ] || return 1
  while IFS= read -r line; do
    seg_ok "$line" "$2" "$3" || return 1
  done <<EOF
$lines
EOF
}

verdict() { # <from> <to> -> allowed|denied|error:...
  local out
  out=$("$KLITE" policy check "$1" "$2" 2>&1)
  case "$out" in
    *"$1 -> $2: allowed"*) echo allowed ;;
    *"$1 -> $2: denied"*)  echo denied ;;
    *) echo "error: $out" ;;
  esac
}

# assert_pair <from> <to> <want> [budget]: data plane first (sustained log
# evidence), then the control-plane verdict, and they must agree. Sets
# LAST_WAIT_S to the observed data-plane settle time.
LAST_WAIT_S=0
assert_pair() {
  local from=$1 to=$2 want=$3 budget=${4:-20} t0 v inst
  inst=$(client_inst "$from")
  t0=$(date +%s)
  if ! wait_for "$budget" pair_is "$from" "$to" "$want"; then
    echo "--- $from's recent traffic:"
    "$KLITE" logs "$inst" --tail 6 2>&1
    echo "--- policy check says: $(verdict "$from" "$to")"
    die "data plane: $from -> $to did not settle to $want within ${budget}s"
  fi
  LAST_WAIT_S=$(( $(date +%s) - t0 ))
  v=$(verdict "$from" "$to")
  if [ "$v" != "$want" ]; then
    echo "--- $from's recent traffic:"
    "$KLITE" logs "$inst" --tail 4 2>&1
    die "PLANES DISAGREE on $from -> $to: data plane shows $want, policy check says $v"
  fi
  pass "$from -> $to: data plane $want (settled ${LAST_WAIT_S}s), policy check agrees"
}

start_agent() {
  "$BIN/klite-agent" --node "$1" --server "$EP" --cluster-token "$TOKEN" \
    >"$WORK/agent-$1.log" 2>&1 &
  echo $! >"$WORK/agent-$1.pid"
  disown
}

# ============================================================
STEP=prep
mkdir -p "$WORK" "$BIN"
teardown # clear leftovers from any previous m6 run
rm -f "$WORK"/agent-m6-*.pid "$WORK"/klited.pid "$WORK"/*.log

for p in 6443 5379 5381 5383; do
  if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    die "port $p is already in use, and the m6 stack needs it free"
  fi
done
pass "m6 control-plane ports free (6443 5379 5381 5383)"

# Pick two node indexes whose donor host ports (19000+i, 19500+i), donor IPs
# (10.44.0.<10+i>), and M9 ingress slices (20000+32*(i-1) .. +31, ADR 0034,
# donors publish the whole slice at creation) are unclaimed. Other stacks
# hold 1..4, so start at 8.
ingress_slice_busy() { # ingress_slice_busy <index>
  local lo=$((20000 + 32 * ($1 - 1))) p
  for p in $(seq "$lo" $((lo + 31))); do
    lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && return 0
  done
  return 1
}
IDX1="" IDX2=""
for i in $(seq 8 40); do
  lsof -nP -iTCP:$((19000 + i)) -sTCP:LISTEN >/dev/null 2>&1 && continue
  lsof -nP -iTCP:$((19500 + i)) -sTCP:LISTEN >/dev/null 2>&1 && continue
  docker network inspect klite0 2>/dev/null | grep -q "\"10\.44\.0\.$((10 + i))/" && continue
  ingress_slice_busy "$i" && continue
  if [ -z "$IDX1" ]; then IDX1=$i; elif [ -z "$IDX2" ]; then IDX2=$i; break; fi
done
[ -n "$IDX1" ] && [ -n "$IDX2" ] || die "no free node-index slots in 8..40"
pass "node indexes picked: m6-1=$IDX1 m6-2=$IDX2 (admin ports $((19000 + IDX1))/$((19000 + IDX2)), donor IPs 10.44.0.$((10 + IDX1))/10.44.0.$((10 + IDX2)), ingress slices $((20000 + 32 * (IDX1 - 1)))+/$((20000 + 32 * (IDX2 - 1)))+)"

# Build from committed HEAD into a private bin dir: the working tree may hold
# another milestone's in-flight edits, and the shared bin/ belongs to whoever
# ran make last. M6_TREE=1 opts into building the tree instead.
build_set() { (cd "$1" && go build -o "$BIN/klited" ./cmd/klited && go build -o "$BIN/klite" ./cmd/klite && go build -o "$BIN/klite-agent" ./cmd/klite-agent); }
if [ "${M6_TREE:-0}" = 1 ] && build_set "$REPO" >"$WORK/build.log" 2>&1; then
  pass "built klited, klite, klite-agent into $BIN from the working tree (M6_TREE=1)"
else
  [ "${M6_TREE:-0}" = 1 ] && info "working tree build failed, using committed HEAD instead"
  rm -rf "$WORK/src" && mkdir -p "$WORK/src"
  git -C "$REPO" archive HEAD | tar -x -C "$WORK/src" || die "git archive HEAD"
  build_set "$WORK/src" >"$WORK/build.log" 2>&1 || die "committed HEAD does not build (see $WORK/build.log)"
  pass "built klited, klite, klite-agent into $BIN from committed HEAD"
fi

for img in klite-net:dev envoyproxy/envoy:v1.31.5 traefik/whoami:v1.10 alpine:3.20; do
  docker image inspect "$img" >/dev/null 2>&1 || die "image $img missing (run make net-image / docker pull when no other stack is mid-run)"
done
pass "required images present"

# Nothing may already answer on the VIP slice this script claims.
if docker network inspect klite0 >/dev/null 2>&1; then
  RESPONDER=$(docker run --rm --network klite0 alpine:3.20 sh -c \
    "for n in 1 2 3 4 5 6 7 8; do ping -c1 -W1 $VIP_SLICE_PREFIX.\$n >/dev/null 2>&1 && echo $VIP_SLICE_PREFIX.\$n; done" 2>/dev/null)
  [ -z "$RESPONDER" ] || die "VIP slice $VIP_SLICE_PREFIX.1-8 already answers on klite0: $RESPONDER"
  pass "VIP slice $VIP_SLICE_PREFIX.1-8 silent on klite0"
else
  info "klite0 does not exist yet, agents will create it"
fi

# Snapshot other stacks' infra so the end of the run can show they survived.
docker ps --format '{{.Names}}' | grep -E '^klite\.(node|m7)-' | sort >"$WORK/other-stacks.before" || true

# ============================================================
STEP=1-boot
ETCD_NAME_PREFIX=etcd-m6 ETCD_PORT_BASE=5379 ETCD_NET=klite-etcd-m6 \
  hack/etcd-up.sh >"$WORK/etcd-up.log" 2>&1 \
  && pass "etcd trio etcd-m6-1..3 healthy on $ETCD_EPS" \
  || die "etcd trio up (see $WORK/etcd-up.log)"

"$BIN/klited" --listen "$EP" --etcd "$ETCD_EPS" --cluster-token "$TOKEN" \
  >"$WORK/klited.log" 2>&1 &
KLITED_PID=$!
echo "$KLITED_PID" >"$WORK/klited.pid"
disown
wait_for 15 klited_serves && pass "klited serving on $EP" || die "klited serving on $EP"

ROWS=0
for k in nodes workloads instances networkpolicies vipallocations; do
  ROWS=$((ROWS + $("$KLITE" get "$k" 2>/dev/null | tail -n +2 | grep -c .)))
done
[ "$ROWS" = 0 ] && pass "fresh etcd store is empty" \
  || die "fresh store already holds $ROWS object(s), so teardown is leaking state"

"$KLITE" apply -f - >/dev/null <<'EOF' || die "apply node YAMLs"
apiVersion: klite/v1
kind: Node
metadata:
  name: m6-1
  labels:
    zone: local
spec:
  maxInstances: 32
---
apiVersion: klite/v1
kind: Node
metadata:
  name: m6-2
  labels:
    zone: local
spec:
  maxInstances: 32
EOF
pass "applied node YAMLs for m6-1 m6-2"

inject_node_index m6-1 "$IDX1" || die "pre-seed status.nodeIndex=$IDX1 on m6-1"
inject_node_index m6-2 "$IDX2" || die "pre-seed status.nodeIndex=$IDX2 on m6-2"
sleep 1 # let any racing controller write land, then re-verify both stuck
node_index_is m6-1 "$IDX1" && node_index_is m6-2 "$IDX2" \
  && pass "node indexes pre-seeded and stable (m6-1=$IDX1 m6-2=$IDX2)" \
  || die "pre-seeded node indexes did not stick"

# ============================================================
STEP=2-vips
# Services land first, alone: the allocator materializes 8 (service, node)
# VIPs which get rewritten into this run's slice BEFORE any donor exists to
# bind them on the shared bridge.
"$KLITE" apply -f - >/dev/null <<'EOF' || die "apply services a b c d"
apiVersion: klite/v1
kind: Service
metadata:
  name: a
spec:
  selector:
    app: a
  port: 8080
  targetPort: 80
---
apiVersion: klite/v1
kind: Service
metadata:
  name: b
spec:
  selector:
    app: b
  port: 8080
  targetPort: 80
---
apiVersion: klite/v1
kind: Service
metadata:
  name: c
spec:
  selector:
    app: c
  port: 8080
  targetPort: 80
---
apiVersion: klite/v1
kind: Service
metadata:
  name: d
spec:
  selector:
    app: d
  port: 8080
  targetPort: 80
EOF
pass "applied services a b c d"

wait_for 20 all_allocs_exist \
  && pass "8 VIP allocations materialized" || die "8 VIP allocations materialized"

N=0
for s in a b c d; do
  for node in $NODES; do
    N=$((N + 1))
    rewrite_vip "$s" "$node" "$VIP_SLICE_PREFIX.$N" || die "rewrite VIP for $s.$node"
  done
done
sleep 1 # controller tick, which must not fight the rewrite
N=0
for s in a b c d; do
  for node in $NODES; do
    N=$((N + 1))
    vip_is "$s" "$node" "$VIP_SLICE_PREFIX.$N" || die "VIP rewrite for $s.$node did not stick"
  done
done
pass "all 8 VIPs rewritten into $VIP_SLICE_PREFIX.1-8 and stable"

# ============================================================
STEP=3-nodes-up
for n in $NODES; do start_agent "$n"; done
wait_for 20 nodes_ready && pass "2 nodes Ready" || die "2 nodes Ready"

node_index_is m6-1 "$IDX1" && node_index_is m6-2 "$IDX2" \
  && pass "Register kept the pre-seeded indexes" \
  || die "Register reassigned node indexes, so isolation is broken. Aborting"

wait_for 60 infra_up \
  && pass "infra pods up (klite.m6-*.net + klite.m6-*.envoy)" || die "infra pods up"

for spec in "m6-1 $IDX1" "m6-2 $IDX2"; do
  set -- $spec
  IP=$(docker inspect -f '{{(index .NetworkSettings.Networks "klite0").IPAddress}}' "klite.$1.net" 2>/dev/null)
  [ "$IP" = "10.44.0.$((10 + $2))" ] || die "klite.$1.net donor IP is $IP, wanted 10.44.0.$((10 + $2))"
done
pass "donor IPs match the pre-seeded indexes (no other stack's address touched)"

# ============================================================
STEP=4-baseline
"$KLITE" apply -f - >/dev/null <<'EOF' || die "apply workloads a b c"
apiVersion: klite/v1
kind: Workload
metadata:
  name: a
  labels:
    app: a
spec:
  replicas: 1
  template:
    labels:
      app: a
    containers:
      - name: client
        image: alpine:3.20
        command: ["/bin/sh", "-c"]
        args:
          - |
            while true; do
              rb=$(wget -qO- -T 2 http://b:8080 2>&1 | grep -m1 Hostname || echo "b FAILED")
              rc=$(wget -qO- -T 2 http://c:8080 2>&1 | grep -m1 Hostname || echo "c FAILED")
              echo "$(date +%T) b => $rb | c => $rc"
              sleep 1
            done
---
apiVersion: klite/v1
kind: Workload
metadata:
  name: b
  labels:
    app: b
spec:
  replicas: 2
  template:
    labels:
      app: b
    containers:
      - name: web
        image: traefik/whoami:v1.10
        env:
          - name: WHOAMI_NAME
            value: b
        ports:
          - containerPort: 80
        readinessProbe:
          tcpPort: 80
---
apiVersion: klite/v1
kind: Workload
metadata:
  name: c
  labels:
    app: c
spec:
  replicas: 2
  template:
    labels:
      app: c
    containers:
      - name: web
        image: traefik/whoami:v1.10
        env:
          - name: WHOAMI_NAME
            value: c
        ports:
          - containerPort: 80
        readinessProbe:
          tcpPort: 80
EOF
pass "applied workloads a (client), b, c (whoami x2)"

wait_for 90 abc_ready \
  && pass "instances Ready (a=1, b=2, c=2)" \
  || { "$KLITE" get instances; die "instances Ready (a=1, b=2, c=2)"; }

A_INST=$("$KLITE" get instances | awk '$2=="a" {print $1}' | head -1)
A_NODE=$("$KLITE" get instances | awk '$2=="a" {print $3}' | head -1)
D_INST="" # set when d lands in step 6
[ -n "$A_INST" ] && [ -n "$A_NODE" ] || die "resolve a's instance and node"
A_CTR="klite.$A_NODE.$A_INST"

# Scenario 1: default allow on both planes, and DNS already answers from the
# rewritten VIP slice, proof the surgery reached the data plane.
assert_pair a b allowed 45
assert_pair a c allowed 45
NSLOOKUP=$(docker exec "$A_CTR" nslookup b.svc.klite 2>&1)
echo "$NSLOOKUP" | grep -q "Address: $VIP_SLICE_PREFIX\." \
  && pass "nslookup b in a's container answers from the rewritten slice ($VIP_SLICE_PREFIX.x)" \
  || { echo "$NSLOOKUP"; die "nslookup b answers from $VIP_SLICE_PREFIX.x"; }

OUT=$("$KLITE" policy check a c 2>&1)
echo "$OUT" | grep -q "default allow" \
  && pass "policy check a c explains the default: $OUT" \
  || info "policy check a c reason: $OUT"

# ============================================================
STEP=5-deny
"$KLITE" apply -f examples/demo-policies/deny-a-to-c.yaml >/dev/null \
  || die "apply deny-a-to-c"
assert_pair a c denied 10   # scenario 2 wants ~5s, budget 10 with the settle time reported
DENY_LAG=$LAST_WAIT_S
assert_pair a b allowed 10  # unrelated pair keeps flowing

OUT=$("$KLITE" policy check a c 2>&1)
echo "$OUT" | grep -q "deny-a-to-c" \
  && pass "denial names the policy: $OUT" \
  || { echo "$OUT"; die "policy check a c names deny-a-to-c"; }

# ADR 0017: existence stays public. The name resolves, the connection resets.
NSLOOKUP=$(docker exec "$A_CTR" nslookup c.svc.klite 2>&1)
echo "$NSLOOKUP" | grep -q "Address: $VIP_SLICE_PREFIX\." \
  && pass "DNS for c still resolves from a's container while denied (ADR 0017)" \
  || { echo "$NSLOOKUP"; die "DNS for c resolves while denied"; }
RESET=$(docker exec "$A_CTR" sh -c 'wget -qO- -T 2 http://c:8080 2>&1' || true)
info "denied wget says: ${RESET:-<no output, connection refused/reset>}"

# ============================================================
STEP=6-allow-flip
"$KLITE" apply -f examples/demo-policies/allow-only-a-to-b.yaml >/dev/null \
  || die "apply allow-only-a-to-b"
assert_pair a b allowed 10 # a is on b's allowlist
assert_pair a c denied 10  # deny-a-to-c still holds

# Client d joins to witness the flip: b now rejects non-allowlisted callers.
"$KLITE" apply -f - >/dev/null <<'EOF' || die "apply workload d"
apiVersion: klite/v1
kind: Workload
metadata:
  name: d
  labels:
    app: d
spec:
  replicas: 1
  template:
    labels:
      app: d
    containers:
      - name: client
        image: alpine:3.20
        command: ["/bin/sh", "-c"]
        args:
          - |
            while true; do
              rb=$(wget -qO- -T 2 http://b:8080 2>&1 | grep -m1 Hostname || echo "b FAILED")
              rc=$(wget -qO- -T 2 http://c:8080 2>&1 | grep -m1 Hostname || echo "c FAILED")
              echo "$(date +%T) b => $rb | c => $rc"
              sleep 1
            done
EOF
wait_for 60 d_ready && pass "client workload d Ready" || die "client workload d Ready"
D_INST=$("$KLITE" get instances | awk '$2=="d" {print $1}' | head -1)
[ -n "$D_INST" ] || die "resolve d's instance"

assert_pair d b denied 30  # allowlist mode shuts d out
assert_pair d c allowed 30 # c is not allowlisted, so default allow holds for d
OUT=$("$KLITE" policy check d b 2>&1)
echo "$OUT" | grep -q "allowlist" \
  && pass "d -> b denial explains allowlist mode: $OUT" \
  || info "d -> b reason: $OUT"

# ============================================================
STEP=7-except
"$KLITE" apply -f examples/demo-policies/lockdown-a.yaml >/dev/null \
  || die "apply lockdown-a"
# The except list carves b out of DENY a->*: with a wildcard deny in force,
# a->b flowing at all is the except list working.
sleep 3 # let the new policy reach Envoy before sampling
assert_pair a b allowed 10
sleep 2 # and stay flowing: rules out sampling lines that predate the policy
pair_is a b allowed || die "a -> b stopped flowing once lockdown-a settled, so the except list is broken"
pass "a -> b still flowing 5s after lockdown-a (except list holds)"
assert_pair a c denied 10
assert_pair d b denied 10  # untouched by lockdown-a: still allowlist-shut
assert_pair d c allowed 10 # lockdown-a is from:a only; d unaffected

# ============================================================
STEP=8-removal
for p in deny-a-to-c allow-only-a-to-b lockdown-a; do
  "$KLITE" delete networkpolicy "$p" >/dev/null || die "delete networkpolicy $p"
done
[ "$("$KLITE" get networkpolicies 2>/dev/null | tail -n +2 | grep -c .)" = 0 ] \
  && pass "all policies deleted" || die "networkpolicies list empty after deletes"

assert_pair a c allowed 10 # scenario 5 wants ~5s; settle time reported
RESTORE_LAG=$LAST_WAIT_S
assert_pair a b allowed 10
assert_pair d b allowed 10
assert_pair d c allowed 10

# ============================================================
STEP=9-teardown
OTHERS_LOST=""
if [ -s "$WORK/other-stacks.before" ]; then
  while IFS= read -r name; do
    docker ps --format '{{.Names}}' | grep -qx "$name" || OTHERS_LOST="$OTHERS_LOST $name"
  done <"$WORK/other-stacks.before"
  [ -z "$OTHERS_LOST" ] \
    && pass "other stacks' infra untouched ($(wc -l <"$WORK/other-stacks.before" | tr -d ' ') containers still up)" \
    || info "other stacks' containers gone since prep (their own churn?):$OTHERS_LOST"
fi

teardown
LEFT=$(docker ps -aq --filter name=etcd-m6 | wc -l | tr -d ' ')
LEFT=$((LEFT + $(docker ps -aq --filter name=klite.m6- | wc -l | tr -d ' ')))
for n in $NODES; do
  LEFT=$((LEFT + $(docker ps -aq --filter "label=io.klite.node=$n" | wc -l | tr -d ' ')))
done
pgrep -f "$BIN/" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
docker network inspect klite-etcd-m6 >/dev/null 2>&1 && LEFT=$((LEFT + 1))
[ "$LEFT" = 0 ] && pass "everything m6-scoped torn down" \
  || die "teardown left $LEFT m6 artifact(s) behind"
docker network inspect klite0 >/dev/null 2>&1 \
  && pass "klite0 left in place" || die "klite0 disappeared during teardown"

echo
echo "verify-m6: all gating steps passed"
echo "timings: deny bit in ${DENY_LAG}s, removal restored flow in ${RESTORE_LAG}s (budgets 10s)"
echo "isolation: node indexes $IDX1/$IDX2, VIP slice $VIP_SLICE_PREFIX.1-8, token $TOKEN, klited $EP"
