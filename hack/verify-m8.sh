#!/usr/bin/env bash
# Checks M8 end to end on the canonical stack (klited :7443, etcd 2379/81/83,
# nodes node-1..3). In run order:
#   auth        token mint, wrong-token and tampered-pin rejections
#   join        a fresh-state mTLS join with on-disk proof
#   data plane  Envoy's TLS xDS stream, the admin-port lockdown
#   failover    agents ride out a klited SIGKILL
#   lifecycle   uncordon, then a WAN-shaped join on this machine's LAN address
# Leaves etcd, the klite0 network, images, and ~/.klite/server in place.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
TMP=/tmp/klite-m8
EP_A=127.0.0.1:7443
EP_B=127.0.0.1:7445
BOTH="$EP_A,$EP_B"
NODES=(node-1 node-2 node-3)
SRV_DIR="$HOME/.klite/server"
AGT_DIR="$HOME/.klite/agent"

KLITED_A_PID=""
KLITED_B_PID=""
declare -A AGENT_PID=()
WAN_PID=""
STEP=prep

pass() { echo "PASS [$STEP]: $1"; }
info() { echo "INFO [$STEP]: $1"; }
die()  { echo "FAIL [$STEP]: $1"; exit 1; }

wait_for() { # wait_for <seconds> <fn> [args...]
  local budget=$1; shift
  for _ in $(seq 1 $((budget * 2))); do
    "$@" && return 0
    sleep 0.5
  done
  return 1
}

my_docker_rm() { # canonical nodes plus this script's extras, other stacks untouched
  local n
  for n in "${NODES[@]}" node-wan; do
    docker ps -aq --filter "label=io.klite.node=$n" | xargs docker rm -f >/dev/null 2>&1
  done
  docker rm -f klite.m8-decoy.net >/dev/null 2>&1
}

cleanup() {
  for n in "${!AGENT_PID[@]}"; do kill "${AGENT_PID[$n]}" 2>/dev/null; done
  [[ -n "$WAN_PID" ]] && kill "$WAN_PID" 2>/dev/null
  [[ -n "$KLITED_A_PID" ]] && kill "$KLITED_A_PID" 2>/dev/null
  [[ -n "$KLITED_B_PID" ]] && kill "$KLITED_B_PID" 2>/dev/null
  my_docker_rm
  wait 2>/dev/null
}
trap cleanup EXIT

start_agent() { # start_agent <node> [extra flags...]
  local n=$1; shift
  "$BIN/klite-agent" --node "$n" --server "$BOTH" --token "$TOKEN" "$@" >"$TMP/agent-$n.log" 2>&1 &
  AGENT_PID[$n]=$!
  disown $!
}

klited_a_ready() { "$KLITE" --server "$EP_A" get workloads >/dev/null 2>&1; }
klited_b_ready() { "$KLITE" --server "$EP_B" get workloads >/dev/null 2>&1; }
nodes_ready()   { [[ "$("$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')" == "$1" ]]; }
node_state()    { "$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk -v n="$1" '$1==n {print $2}'; }
node_cordoned() { "$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk -v n="$1" '$1==n {print $3}'; }
node_gone()     { ! "$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "$1"; }

infra_up() {
  local n
  for n in "${NODES[@]}"; do
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.envoy" || return 1
  done
}

counts_ready() { # a=1, b=<n>, c=3, d=4 all Ready
  local snap; snap="$("$KLITE" --server "$BOTH" get instances 2>/dev/null)"
  [[ "$(echo "$snap" | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == 3 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="d" && $4=="Ready"' | wc -l | tr -d ' ')" == 4 ]]
}

# ============================================================
STEP=prep
mkdir -p "$TMP"
rm -f "$TMP"/*.log
pkill -f 'bin/klited' 2>/dev/null
pkill -f 'bin/klite-agent' 2>/dev/null
sleep 0.5
my_docker_rm
for p in 7443 7445; do
  lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && die "port $p is already in use"
done

# Fresh state end to end: new etcd store, new CA + admin token, no persisted
# node identities — the join in step 4 must start from nothing.
hack/etcd-up.sh down >/dev/null 2>&1
rm -rf "$HOME/.klite/etcd/etcd-1" "$HOME/.klite/etcd/etcd-2" "$HOME/.klite/etcd/etcd-3"
rm -rf "$SRV_DIR"
for n in "${NODES[@]}" node-wan m8-neg; do rm -rf "$AGT_DIR/$n/tls"; done
hack/etcd-up.sh >/dev/null 2>&1 && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

make build >/dev/null 2>&1 && pass "make build" || die "make build"
if ! docker image inspect klite-net:dev >/dev/null 2>&1; then
  make net-image >/dev/null 2>&1 || die "make net-image"
fi
docker image inspect alpine:3.20 >/dev/null 2>&1 || docker pull alpine:3.20 >/dev/null 2>&1

# ============================================================
STEP=1-boot
# A binds all interfaces so the WAN-shaped join in step 9 can reach it. B is
# the loopback standby both CLI and agents fail over to.
"$BIN/klited" --listen 0.0.0.0:7443 >"$TMP/klited-a.log" 2>&1 &
KLITED_A_PID=$!
disown
wait_for 15 klited_a_ready && pass "klited A serving on $EP_A (TLS)" || die "klited A serving on $EP_A"
"$BIN/klited" --listen "$EP_B" >"$TMP/klited-b.log" 2>&1 &
KLITED_B_PID=$!
disown
wait_for 15 klited_b_ready && pass "klited B serving on $EP_B (shared CA + admin token)" || die "klited B serving on $EP_B"

[[ "$(stat -f '%Lp' "$SRV_DIR/token")" == "600" ]] \
  && pass "admin token minted 0600 at $SRV_DIR/token" || die "admin token file missing or not 0600"
[[ -f "$SRV_DIR/tls/ca.crt" && -f "$SRV_DIR/tls/ca.key" ]] \
  && pass "CA materialized under $SRV_DIR/tls" || die "CA files missing"
CLUSTER_ID="$(grep -o 'clusterID=[0-9a-f]*' "$TMP/klited-a.log" | head -1 | cut -d= -f2)"
[[ -n "$CLUSTER_ID" ]] && pass "cluster id minted at first boot ($CLUSTER_ID)" || die "cluster id in klited log"

# ============================================================
STEP=2-admin-auth
TOKEN="$("$KLITE" --server "$BOTH" node token)" || die "klite node token"
[[ "$TOKEN" =~ ^K10[0-9a-f]{64}::node:.+$ ]] \
  && pass "join token has the K10<ca-sha256>::node:<secret> shape" \
  || die "token shape unexpected: $TOKEN"

KLITE_TOKEN=deadbeef "$KLITE" --server "$BOTH" get nodes >/dev/null 2>"$TMP/badtoken.err" \
  && die "a wrong admin token was accepted"
grep -q "admin token" "$TMP/badtoken.err" \
  && pass "wrong admin token rejected with a pointed message" \
  || die "wrong-admin-token error unclear: $(cat "$TMP/badtoken.err")"

KLITE_CA=/nonexistent "$KLITE" --server "$BOTH" get nodes >/dev/null 2>&1 \
  && die "CLI dialed without any CA to verify against"
KLITE_CA=/nonexistent "$KLITE" --server "$BOTH" --insecure get nodes >/dev/null 2>&1 \
  && pass "CA required by default, with --insecure as the explicit escape hatch" \
  || die "--insecure escape hatch broken"

# ============================================================
STEP=3-mtls-server-chain
SCLIENT="$(echo | openssl s_client -connect "$EP_A" -CAfile "$SRV_DIR/tls/ca.crt" -showcerts 2>/dev/null)"
echo "$SCLIENT" | grep -q "Verify return code: 0 (ok)" \
  && pass "klited's serving cert verifies against the cluster CA" \
  || die "serving cert does not verify against ca.crt"
echo "$SCLIENT" | grep -q "TLSv1.3" \
  && pass "listener negotiates TLS 1.3" || die "listener did not negotiate TLS 1.3"
CHAIN_LEN="$(echo "$SCLIENT" | grep -c 'BEGIN CERTIFICATE')"
[[ "$CHAIN_LEN" == 2 ]] \
  && pass "server presents the [leaf, CA] chain bootstrap pinning depends on" \
  || die "server chain has $CHAIN_LEN certs, want 2"

# ============================================================
STEP=4-join-rejections
HASH="${TOKEN:3:64}"
SECRET="${TOKEN##*::node:}"
WRONG_SECRET="K10${HASH}::node:not-the-secret"
"$BIN/klite-agent" --node m8-neg --server "$EP_A" --token "$WRONG_SECRET" --state-dir "$TMP/neg" >"$TMP/neg-secret.log" 2>&1 &
NEG_PID=$!
if wait "$NEG_PID" 2>/dev/null; then die "agent with a wrong join secret exited 0"; fi
grep -q "join rejected" "$TMP/neg-secret.log" \
  && pass "wrong join secret rejected (agent exits, nothing persisted)" \
  || die "wrong-secret rejection not logged: $(tail -2 "$TMP/neg-secret.log")"

FLIP="a"; [[ "${HASH:0:1}" == "a" ]] && FLIP="b"
TAMPERED="K10${FLIP}${HASH:1}::node:${SECRET}"
"$BIN/klite-agent" --node m8-neg --server "$EP_A" --token "$TAMPERED" --state-dir "$TMP/neg" >"$TMP/neg-pin.log" 2>&1 &
NEG_PID=$!
if wait "$NEG_PID" 2>/dev/null; then die "agent with a tampered CA pin exited 0"; fi
grep -q "pinned CA" "$TMP/neg-pin.log" \
  && pass "tampered CA hash rejected before any credential leaves the agent" \
  || die "tampered-pin rejection not logged: $(tail -2 "$TMP/neg-pin.log")"
[[ -e "$TMP/neg/m8-neg/tls/node.key" ]] && die "rejected join still persisted an identity"
pass "no identity persisted by either rejected join"

# ============================================================
STEP=5-fresh-join
for i in 1 2 3; do
  "$KLITE" --server "$BOTH" apply -f "examples/seed/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
for n in "${NODES[@]}"; do start_agent "$n"; done
wait_for 30 nodes_ready 3 && pass "3 nodes joined and Ready" || die "3 nodes Ready"

for n in "${NODES[@]}"; do
  d="$AGT_DIR/$n/tls"
  for f in node.key node.crt ca.crt; do
    [[ "$(stat -f '%Lp' "$d/$f" 2>/dev/null)" == "600" ]] || die "$d/$f missing or not 0600"
  done
  openssl verify -CAfile "$SRV_DIR/tls/ca.crt" "$d/node.crt" >/dev/null 2>&1 \
    || die "$n's cert does not chain to the cluster CA"
  openssl x509 -in "$d/node.crt" -noout -subject 2>/dev/null | grep -q "klite:node:$n" \
    || die "$n's cert CN is not klite:node:$n"
done
pass "per-node identities on disk: 0600 trio, CA-chained, CN=klite:node:<name>"
grep -q "joined: node identity persisted" "$TMP/agent-node-1.log" \
  && pass "agents joined via token (fresh state, no reuse)" || die "agent-1 log shows a token join"

wait_for 60 infra_up && pass "infra pods up on all nodes" || die "infra pods up"
for n in "${NODES[@]}"; do
  got="$(docker inspect "klite.$n.net" --format '{{index .Config.Labels "io.klite.cluster"}}')"
  [[ "$got" == "$CLUSTER_ID" ]] || die "donor of $n carries cluster label '$got', want $CLUSTER_ID"
done
pass "infra containers labeled io.klite.cluster=$CLUSTER_ID"

# ============================================================
STEP=6-envoy-mtls
for app in a-client b-whoami c-whoami d-web; do
  "$KLITE" --server "$BOTH" apply -f "examples/seed/apps/$app.yaml" >/dev/null || die "apply $app.yaml"
done
wait_for 90 counts_ready 2 && pass "workloads a, b, c, d all Ready (probe-gated)" || die "workloads Ready"

for i in 1 2 3; do
  CS="$(curl -s --max-time 3 "127.0.0.1:$((19500 + i))/stats" 2>/dev/null | awk '/^control_plane.connected_state/ {print $2}')"
  [[ "$CS" == 1 ]] || die "envoy on node-$i not connected to xDS over TLS (connected_state=$CS)"
done
pass "every Envoy holds its ADS stream over node-cert mTLS (connected_state=1)"

A_INST="$("$KLITE" --server "$BOTH" get instances | awk '$2=="a" {print $1}' | head -1)"
A_NODE="$("$KLITE" --server "$BOTH" get instances | awk '$2=="a" {print $3}' | head -1)"
# A burst of probes from inside a's container rides kdns -> VIP -> Envoy.
# Bodies read "<cid> is b", so two distinct cids prove both endpoints answer.
# The seeded apps' own 2.5% chatter is too sparse to gate on.
b_lb() {
  local i
  [[ "$(for i in $(seq 1 10); do
          docker exec "klite.$A_NODE.$A_INST" wget -qO- -T 2 http://b:8080 2>/dev/null
        done | awk '$2=="is" && $3=="b" {print $1}' | sort -u | wc -l | tr -d ' ')" -ge 2 ]]
}
wait_for 45 b_lb && pass "data plane flows: probes from a balance across b's endpoints" || die "traffic through the TLS-bootstrapped Envoy"

# ============================================================
STEP=7-admin-lockdown
# Empirical baseline (this machine, colima): docker-proxy dials the donor
# from the klite0 gateway 10.44.0.1, outside both denied source ranges.
PROBE() { # PROBE <ip> <port>  -> 0 if reachable from a workload-range container
  docker run --rm --network klite0 alpine:3.20 nc -z -w 2 "$1" "$2" >/dev/null 2>&1
}
for i in 1 2 3; do
  ip="10.44.0.$((10 + i))"
  PROBE "$ip" 9090 && die "workload-range source reached klite-net admin at $ip:9090"
  PROBE "$ip" 9901 && die "workload-range source reached Envoy admin at $ip:9901"
done
pass "klite-net :9090 and Envoy :9901 unreachable from the workload range on all 3 donors"
for i in 1 2 3; do
  nc -z -w 2 127.0.0.1 "$((19000 + i))" >/dev/null 2>&1 || die "host path to klite-net admin 1900$i broken"
  [[ "$(curl -s --max-time 2 -o /dev/null -w '%{http_code}' "127.0.0.1:$((19500 + i))/ready")" == 200 ]] \
    || die "host path to Envoy admin 1950$i broken"
done
pass "host (docker-proxy) path to both admin planes still works"

# A parked donor from another cluster is never cleanup fodder.
docker run -d --name klite.m8-decoy.net \
  --label io.klite.role=net --label io.klite.node=m8-decoy --label io.klite.cluster=m8-foreign \
  --network klite0 alpine:3.20 sleep 120 >/dev/null || die "start decoy donor"
sleep 6
docker ps --format '{{.Names}}' | grep -qx klite.m8-decoy.net \
  && pass "foreign-cluster donor left untouched through reconcile passes" \
  || die "agents removed a foreign cluster's donor"
docker rm -f klite.m8-decoy.net >/dev/null 2>&1

# ============================================================
STEP=8-klited-failover
B_INSTANCES="$("$KLITE" --server "$EP_B" get instances | awk '$2=="b"' | wc -l | tr -d ' ')"
[[ "$B_INSTANCES" == 2 ]] || die "expected b at 2 before the failover scale"
kill -9 "$KLITED_A_PID" || die "SIGKILL klited A"
info "SIGKILLed klited A, so agents' streams must resume on B"
T0=$(date +%s)
DIP=""
for _ in $(seq 1 15); do
  [[ "$("$KLITE" --server "$EP_B" get nodes 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 3 ]] || DIP=1
  sleep 1
done
[[ -z "$DIP" ]] && pass "all 3 nodes stayed Ready for 15s after the kill (heartbeats failed over)" \
  || die "a node dipped from Ready after klited A died"

"$KLITE" --server "$EP_B" scale workload b --replicas 3 >/dev/null || die "scale through the survivor"
b3() { counts_ready 3; }
wait_for 45 b3 && pass "scale to 3 converged ($(( $(date +%s) - T0 ))s after the kill): desired-state streams live on B" \
  || die "scale did not converge after failover"
RESUMED=0
for n in "${NODES[@]}"; do
  grep -q "stream broke, reconnecting" "$TMP/agent-$n.log" && RESUMED=1
done
[[ "$RESUMED" == 1 ]] && pass "at least one agent logged a broken stream and carried on" \
  || info "no agent stream sat on A at kill time (round-robin landed them all on B)"

LOG_A2="$TMP/klited-a2.log"
"$BIN/klited" --listen 0.0.0.0:7443 >"$LOG_A2" 2>&1 &
KLITED_A_PID=$!
disown
wait_for 15 klited_a_ready && pass "klited A restarted for the WAN step" || die "klited A restart"

# ============================================================
STEP=9-uncordon
"$KLITE" --server "$BOTH" drain node-3 >"$TMP/drain.log" 2>&1 || die "drain node-3"
[[ "$(node_cordoned node-3)" == "true" ]] && pass "node-3 cordoned by the drain" || die "node-3 cordoned"
"$KLITE" --server "$BOTH" uncordon node-3 >/dev/null || die "klite uncordon node-3"
[[ "$(node_cordoned node-3)" == "false" ]] && pass "uncordon cleared unschedulable" || die "uncordon cleared unschedulable"
"$KLITE" --server "$BOTH" scale workload b --replicas 5 >/dev/null || die "scale b to 5"
on_node3() { "$KLITE" --server "$BOTH" get instances 2>/dev/null | awk '$3=="node-3" && ($4=="Ready" || $4=="Running")' | grep -q .; }
wait_for 60 on_node3 && pass "new instances scheduled onto the uncordoned node" \
  || die "nothing scheduled onto node-3 after uncordon"

# ============================================================
STEP=10-wan-join
WAN_IP="$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null)"
if [[ -z "$WAN_IP" ]]; then
  info "no en0/en1 address, skipping the WAN-shaped join (needs a LAN interface)"
else
  printf 'apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: node-wan\n  labels:\n    zone: wan\nspec:\n  maxInstances: 8\n' \
    | "$KLITE" --server "$BOTH" apply -f - >/dev/null || die "apply node-wan"
  "$BIN/klite-agent" --node node-wan --server "$WAN_IP:7443" --token "$TOKEN" >"$TMP/agent-wan.log" 2>&1 &
  WAN_PID=$!
  disown
  wan_ready() { [[ "$(node_state node-wan)" == "Ready" ]]; }
  wait_for 45 wan_ready \
    && pass "node-wan joined Ready through $WAN_IP:7443 (token pin + interface SAN)" \
    || die "WAN-shaped join failed (see $TMP/agent-wan.log)"
  openssl verify -CAfile "$SRV_DIR/tls/ca.crt" "$AGT_DIR/node-wan/tls/node.crt" >/dev/null 2>&1 \
    && pass "node-wan holds a CA-chained identity like any local node" \
    || die "node-wan identity invalid"
  kill "$WAN_PID" 2>/dev/null
  WAN_PID=""
  "$KLITE" --server "$BOTH" delete node node-wan >/dev/null 2>&1
  wait_for 30 node_gone node-wan && pass "node-wan drained out and removed" || die "node-wan removal"
fi

# ============================================================
STEP=teardown
cleanup
trap - EXIT
LEFT=$(docker ps -aq --filter label=io.klite.node=node-wan | wc -l | tr -d ' ')
pgrep -f "bin/klited" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
pgrep -f "bin/klite-agent" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
[[ "$LEFT" == 0 ]] && pass "processes and node-wan artifacts torn down (etcd, klite0, images, ~/.klite/server stay)" \
  || die "teardown left $LEFT artifact(s)"

echo
echo "verify-m8: all steps passed"
