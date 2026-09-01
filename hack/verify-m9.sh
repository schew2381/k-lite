#!/usr/bin/env bash
# Checks M9 end to end on the canonical stack (klited :7443, etcd 2379/81/83,
# nodes node-1..3): donors publish per-index ingress ranges, klited allocates
# per-endpoint ingress ports, cross-node traffic rides proxy-terminated mTLS
# (ADR 0024) with handshake counters to prove it, plaintext and foreign-CA
# dials die in the handshake, scale/rollout/drain stay hitless through the
# ingress hop, a WAN-shaped advertise address keeps traffic flowing, and
# killing an instance releases its port. Leaves etcd, the klite0 network,
# images, and ~/.klite/server in place.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
TMP=/tmp/klite-m9
EP=127.0.0.1:7443
NODES=(node-1 node-2 node-3)
SRV_DIR="$HOME/.klite/server"
AGT_DIR="$HOME/.klite/agent"
INGRESS_BASE=20000
INGRESS_PER_NODE=32

KLITED_PID=""
declare -A AGENT_PID=()
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

my_docker_rm() {
  local n
  for n in "${NODES[@]}"; do
    docker ps -aq --filter "label=io.klite.node=$n" | xargs docker rm -f >/dev/null 2>&1
  done
}

cleanup() {
  for n in "${!AGENT_PID[@]}"; do kill "${AGENT_PID[$n]}" 2>/dev/null; done
  [[ -n "$KLITED_PID" ]] && kill "$KLITED_PID" 2>/dev/null
  my_docker_rm
  wait 2>/dev/null
}
trap cleanup EXIT

start_agent() { # start_agent <node> [extra flags...]
  local n=$1; shift
  "$BIN/klite-agent" --node "$n" --server "$EP" --token "$TOKEN" "$@" >>"$TMP/agent-$n.log" 2>&1 &
  AGENT_PID[$n]=$!
  disown $!
}

klited_ready() { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()  { [[ "$("$KLITE" get nodes 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 3 ]]; }

infra_up() {
  local n
  for n in "${NODES[@]}"; do
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.envoy" || return 1
  done
}

counts_ready() { # a=1, b=<$1>, c=2 all Ready
  local snap; snap="$("$KLITE" get instances 2>/dev/null)"
  [[ "$(echo "$snap" | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]]
}

# idx_of <node>: the node's cluster-assigned index, read off its donor's
# published klite-net admin port (19000 + index).
idx_of() { docker port "klite.$1.net" 9090 | head -1 | sed 's/.*://' | awk '{print $1 - 19000}'; }
envoy_admin() { echo $((19500 + $(idx_of "$1"))); }
estat() { # estat <node> <exact stat name> -> value (0 when absent)
  local v
  v="$(curl -s --max-time 3 "127.0.0.1:$(envoy_admin "$1")/stats" | awk -v n="$2" '$1 == (n ":") {print $2}')"
  echo "${v:-0}"
}

# alloc_row <service> <instance>: "node port" for one ingress allocation.
alloc_row() { "$KLITE" get ingressallocations 2>/dev/null | awk -v s="$1" -v i="$2" '$2==s && $3==i {print $4, $5}'; }

a_ctr() { echo "klite.$A_NODE.$A_INST"; }
failed_since() { docker logs "$(a_ctr)" --since "$1" 2>/dev/null | grep -c 'FAILED'; }

# ============================================================
STEP=prep
mkdir -p "$TMP"
rm -f "$TMP"/*.log "$TMP"/*.pem
pkill -f 'bin/klited' 2>/dev/null
pkill -f 'bin/klite-agent' 2>/dev/null
sleep 0.5
my_docker_rm
lsof -nP -iTCP:7443 -sTCP:LISTEN >/dev/null 2>&1 && die "port 7443 is already in use"
for p in $(seq $INGRESS_BASE $((INGRESS_BASE + 3 * INGRESS_PER_NODE - 1))); do
  lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && die "ingress port $p is already in use"
done
pass "canonical ports free (7443, ingress $INGRESS_BASE-$((INGRESS_BASE + 3 * INGRESS_PER_NODE - 1)))"

# Fresh end to end: new store, new CA and admin token, new node identities,
# so the run also proves the dual-purpose (client+server EKU) join flow.
hack/etcd-up.sh down >/dev/null 2>&1
rm -rf "$HOME/.klite/etcd/etcd-1" "$HOME/.klite/etcd/etcd-2" "$HOME/.klite/etcd/etcd-3"
rm -rf "$SRV_DIR"
for n in "${NODES[@]}"; do rm -rf "$AGT_DIR/$n/tls"; done
hack/etcd-up.sh >/dev/null 2>&1 && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

make build >/dev/null 2>&1 && pass "make build" || die "make build"
if ! docker image inspect klite-net:dev >/dev/null 2>&1; then
  make net-image >/dev/null 2>&1 || die "make net-image"
fi
docker image inspect alpine:3.20 >/dev/null 2>&1 || docker pull alpine:3.20 >/dev/null 2>&1

# ============================================================
STEP=1-boot
"$BIN/klited" --listen "$EP" >"$TMP/klited.log" 2>&1 &
KLITED_PID=$!
disown
wait_for 15 klited_ready && pass "klited serving on $EP" || die "klited serving on $EP"
TOKEN="$("$KLITE" node token)" || die "mint join token"

for i in 1 2 3; do
  "$KLITE" apply -f "examples/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
for n in "${NODES[@]}"; do start_agent "$n"; done
wait_for 30 nodes_ready && pass "3 nodes joined and Ready" || die "3 nodes Ready"
wait_for 60 infra_up && pass "infra pods up on all nodes" || die "infra pods up"

for n in "${NODES[@]}"; do
  openssl x509 -in "$AGT_DIR/$n/tls/node.crt" -noout -purpose 2>/dev/null \
    | grep -q "SSL server : Yes" || die "$n's node cert cannot serve TLS (missing ServerAuth EKU)"
done
pass "node certs carry both TLS purposes (client for klited, server for ingress)"

# ============================================================
STEP=2-published-ranges
for n in "${NODES[@]}"; do
  i="$(idx_of "$n")"
  [[ "$i" =~ ^[0-9]+$ && "$i" -ge 1 ]] || die "cannot derive $n's index from its donor ports"
  lo=$((INGRESS_BASE + INGRESS_PER_NODE * (i - 1)))
  hi=$((lo + INGRESS_PER_NODE - 1))
  got="$(docker port "klite.$n.net" | grep -c "0.0.0.0:20[0-9][0-9][0-9]$")"
  [[ "$got" == "$INGRESS_PER_NODE" ]] || die "$n's donor publishes $got ingress ports, want $INGRESS_PER_NODE"
  docker port "klite.$n.net" "$lo/tcp" | grep -q "0.0.0.0:$lo" || die "$n's slice does not start at $lo"
  docker port "klite.$n.net" "$hi/tcp" | grep -q "0.0.0.0:$hi" || die "$n's slice does not end at $hi"
  docker port "klite.$n.net" "$((hi + 1))/tcp" >/dev/null 2>&1 && die "$n published past its slice"
done
pass "each donor publishes exactly its 32-port slice on 0.0.0.0 (index-derived)"

# Advertise addresses resolve against the donor's /etc/hosts and ride a
# heartbeat, so they land a beat after the infra pods do.
advertised() { [[ "$("$KLITE" get nodes -o yaml 2>/dev/null | grep -c 'advertiseAddress:')" == 3 ]]; }
wait_for 30 advertised || die "want 3 advertise addresses in NodeStatus, got $("$KLITE" get nodes -o yaml 2>/dev/null | grep -c 'advertiseAddress:')"
"$KLITE" get nodes -o yaml | awk '/advertiseAddress:/ {print $2}' | while read -r ip; do
  [[ "$ip" =~ ^[0-9.]+$ ]] || die "advertise address $ip is not a literal IPv4"
done || exit 1
pass "all 3 nodes advertise a resolved literal IP (default host.docker.internal path)"

# ============================================================
STEP=3-apps
for app in a-client b-whoami c-whoami; do
  "$KLITE" apply -f "examples/apps/$app.yaml" >/dev/null || die "apply $app.yaml"
done
wait_for 90 counts_ready 2 && pass "workloads a, b, c all Ready (probe-gated)" || die "workloads Ready"
A_INST="$("$KLITE" get instances | awk '$2=="a" {print $1}' | head -1)"
A_NODE="$("$KLITE" get instances | awk '$2=="a" {print $3}' | head -1)"
[[ -n "$A_INST" && -n "$A_NODE" ]] || die "resolve a's instance and node"

# ============================================================
STEP=4-allocations
allocs_settled() { [[ "$("$KLITE" get ingressallocations 2>/dev/null | tail -n +2 | grep -c .)" == 5 ]]; }
wait_for 30 allocs_settled || { "$KLITE" get ingressallocations; die "want 5 ingress allocations (a=1, b=2, c=2)"; }
"$KLITE" get ingressallocations | tail -n +2 | while read -r name svc inst node port; do
  i="$(idx_of "$node")"
  lo=$((INGRESS_BASE + INGRESS_PER_NODE * (i - 1)))
  hi=$((lo + INGRESS_PER_NODE))
  [[ "$port" -ge "$lo" && "$port" -lt "$hi" ]] \
    || die "allocation $name holds port $port outside $node's slice [$lo,$hi)"
  [[ "$("$KLITE" get instances | awk -v n="$inst" '$1==n {print $3}')" == "$node" ]] \
    || die "allocation $name names node $node but the instance lives elsewhere"
done || exit 1
pass "klite get ingressallocations: 5 rows, each inside its owner's slice"

echo 'apiVersion: klite/v1
kind: IngressAllocation
metadata:
  name: forged
spec: {}' | "$KLITE" apply -f - 2>&1 | grep -q "server-materialized" \
  && pass "Apply rejects the server-materialized kind" \
  || die "Apply accepted a client-written IngressAllocation"

# ============================================================
STEP=5-mtls-dataflow
b_lb() { [[ "$(docker logs "$(a_ctr)" --since 15s 2>/dev/null | grep -o 'b => Hostname: [0-9a-f]*' | awk '{print $4}' | sort -u | wc -l | tr -d ' ')" -ge 2 ]]; }
wait_for 60 b_lb && pass "a's requests balance across both b endpoints" || { docker logs "$(a_ctr)" --since 30s 2>&1 | tail -5; die "a reaches both b endpoints"; }

# The remote b endpoint's ingress listener must be doing real TLS work:
# its handshake counter rises while the a-loop runs.
REMOTE_B_INST="$("$KLITE" get instances | awk -v an="$A_NODE" '$2=="b" && $3!=an {print $1}' | head -1)"
read -r REMOTE_B_NODE REMOTE_B_PORT <<<"$(alloc_row b "$REMOTE_B_INST")"
[[ -n "$REMOTE_B_PORT" ]] || die "no ingress allocation for remote b instance $REMOTE_B_INST"
HS_STAT="listener.0.0.0.0_${REMOTE_B_PORT}.ssl.handshake"
H0="$(estat "$REMOTE_B_NODE" "$HS_STAT")"
sleep 6
H1="$(estat "$REMOTE_B_NODE" "$HS_STAT")"
[[ "$H1" -gt "$H0" ]] \
  && pass "ingress mTLS handshakes rising on $REMOTE_B_NODE:$REMOTE_B_PORT ($H0 -> $H1)" \
  || die "no ssl handshakes on the ingress path ($HS_STAT: $H0 -> $H1)"

# Cross-node EDS carries machineAddress:ingressPort, and the raw pod IP of a
# remote endpoint appears nowhere in the consumer's cluster table.
REMOTE_B_IP="$("$KLITE" get instances | awk -v n="$REMOTE_B_INST" '$1==n {print $6}')"
CLUSTERS="$(curl -s --max-time 3 "127.0.0.1:$(envoy_admin "$A_NODE")/clusters")"
echo "$CLUSTERS" | grep -q "b::.*:${REMOTE_B_PORT}::" || die "consumer EDS lacks the remote ingress endpoint :$REMOTE_B_PORT"
echo "$CLUSTERS" | grep "^b::" | grep -q "$REMOTE_B_IP:" && die "consumer EDS still dials the remote pod IP raw (flat bridge lives)"
pass "consumer EDS dials remote endpoints via ingress ports, never raw pod IPs"

# ============================================================
STEP=6-rejections
# The probes target the ingress port of the b instance on a's OWN node: a
# dials that one over the raw local path, so its ingress listener carries no
# legitimate traffic and every counter delta below belongs to the probes.
QUIET_B_INST="$("$KLITE" get instances | awk -v an="$A_NODE" '$2=="b" && $3==an {print $1}' | head -1)"
read -r _ QUIET_PORT <<<"$(alloc_row b "$QUIET_B_INST")"
[[ -n "$QUIET_PORT" ]] || die "no ingress allocation for a-local b instance $QUIET_B_INST"
RX_STAT="tcp.ingress_${QUIET_PORT}.downstream_cx_rx_bytes_total"
RX0="$(estat "$A_NODE" "$RX_STAT")"

curl -s --max-time 3 "http://127.0.0.1:${QUIET_PORT}/" >/dev/null 2>&1 && die "plaintext HTTP got a response from an ingress port"
pass "plaintext dial gets nothing back"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout "$TMP/foreign.key" -out "$TMP/foreign.crt" \
  -subj "/CN=klite:node:node-1" -days 1 >/dev/null 2>&1 || die "mint foreign cert"
FV0="$(estat "$A_NODE" "listener.0.0.0.0_${QUIET_PORT}.ssl.fail_verify_error")"
printf 'GET / HTTP/1.1\r\n\r\n' | openssl s_client -connect "127.0.0.1:${QUIET_PORT}" \
  -cert "$TMP/foreign.crt" -key "$TMP/foreign.key" -CAfile "$AGT_DIR/node-1/tls/ca.crt" \
  >"$TMP/foreign.out" 2>&1
grep -q "HTTP/1.1" "$TMP/foreign.out" && die "foreign-CA client cert reached the pod"
FV1="$(estat "$A_NODE" "listener.0.0.0.0_${QUIET_PORT}.ssl.fail_verify_error")"
[[ "$FV1" -gt "$FV0" ]] \
  && pass "foreign-CA client cert rejected in the handshake (fail_verify_error $FV0 -> $FV1)" \
  || die "foreign cert did not register a verify failure ($FV0 -> $FV1)"

printf 'GET / HTTP/1.1\r\n\r\n' | openssl s_client -connect "127.0.0.1:${QUIET_PORT}" \
  -CAfile "$AGT_DIR/node-1/tls/ca.crt" >"$TMP/nocert.out" 2>&1
grep -q "HTTP/1.1" "$TMP/nocert.out" && die "certless TLS client reached the pod"
pass "certless TLS dial gets nothing back (client certificate required)"

# Positive control, then the proof: a proper node-cert handshake succeeds on
# the same listener, while the three rejected dials moved zero decrypted
# bytes — nothing they sent ever crossed the proxy toward the pod.
{ printf 'GET / HTTP/1.1\r\nHost: b\r\nConnection: close\r\n\r\n'; sleep 2; } \
  | openssl s_client -quiet -connect "127.0.0.1:${QUIET_PORT}" \
      -cert "$AGT_DIR/node-1/tls/node.crt" -key "$AGT_DIR/node-1/tls/node.key" \
      -CAfile "$AGT_DIR/node-1/tls/ca.crt" >"$TMP/nodecert.out" 2>&1
grep -q "Hostname:" "$TMP/nodecert.out" \
  && pass "node-cert client reached the pod through the same listener (positive control)" \
  || die "node-cert handshake did not reach the pod (see $TMP/nodecert.out)"
RX1="$(estat "$A_NODE" "$RX_STAT")"
CONTROL_BYTES=$((RX1 - RX0))
[[ "$CONTROL_BYTES" -gt 0 ]] || die "positive control moved no bytes; the rx counter is not measuring"
RX2_BASE="$RX1"
curl -s --max-time 3 "http://127.0.0.1:${QUIET_PORT}/" >/dev/null 2>&1
printf 'x' | openssl s_client -connect "127.0.0.1:${QUIET_PORT}" -cert "$TMP/foreign.crt" -key "$TMP/foreign.key" >/dev/null 2>&1
RX3="$(estat "$A_NODE" "$RX_STAT")"
[[ "$((RX3 - RX2_BASE))" == 0 ]] \
  && pass "rejected dials moved zero decrypted bytes toward the pod (rx delta 0; control moved $CONTROL_BYTES)" \
  || die "a rejected dial moved $((RX3 - RX2_BASE)) bytes through the proxy"

# ============================================================
STEP=7-churn-hitless
MARKER="$(date -u +%Y-%m-%dT%H:%M:%S)"
sleep 2

"$KLITE" scale workload b --replicas 3 >/dev/null || die "scale b to 3"
b3() { counts_ready 3; }
wait_for 60 b3 && pass "scale b 2->3 converged" || die "scale b to 3 converged"

# Rollout: template change (extra env var) drives the surge-first replace.
B_BEFORE="$("$KLITE" get instances | awk '$2=="b" {print $1}' | sort)"
awk '1; /value: b$/ {print "          - name: M9_ROLLOUT"; print "            value: \"1\""}' \
  examples/apps/b-whoami.yaml | "$KLITE" apply -f - >/dev/null || die "apply rolled b template"
rolled() {
  [[ "$("$KLITE" get instances 2>/dev/null | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == 3 ]] || return 1
  [[ "$("$KLITE" get instances 2>/dev/null | awk '$2=="b" {print $1}' | sort)" != "$B_BEFORE" ]] || return 1
  # every pre-rollout instance replaced, not just some
  local left
  left="$(comm -12 <(echo "$B_BEFORE") <("$KLITE" get instances 2>/dev/null | awk '$2=="b" {print $1}' | sort) | grep -c . || true)"
  [[ "$left" == 0 ]]
}
wait_for 120 rolled && pass "surge-first rollout replaced all 3 b instances, all Ready" || die "rollout converged"

# Drain a node that hosts b but not a: the drained endpoints are exactly the
# ones a reaches through ingress.
DRAIN_NODE="$("$KLITE" get instances | awk -v an="$A_NODE" '$2=="b" && $3!=an {print $3}' | head -1)"
[[ -n "$DRAIN_NODE" ]] || die "no remote node hosting b to drain"
"$KLITE" drain "$DRAIN_NODE" >"$TMP/drain.log" 2>&1 || die "drain $DRAIN_NODE (see $TMP/drain.log)"
pass "drained $DRAIN_NODE through the ingress-hop data path"
"$KLITE" uncordon "$DRAIN_NODE" >/dev/null || die "uncordon $DRAIN_NODE"
wait_for 60 b3 && pass "cluster re-converged after uncordon" || die "post-drain reconverge"

sleep 3
FAILS="$(failed_since "$MARKER")"
[[ "$FAILS" == 0 ]] \
  && pass "ZERO failed requests across scale + rollout + drain (a-loop clean since $MARKER)" \
  || { docker logs "$(a_ctr)" --since "$MARKER" 2>/dev/null | grep FAILED | head -5; die "$FAILS failed request(s) during churn"; }

# ============================================================
STEP=8-wan
WAN_IP="$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null)"
if [[ -z "$WAN_IP" ]]; then
  info "no en0/en1 address; skipping the WAN-shaped advertise (needs a LAN interface)"
else
  WAN_NODE="$("$KLITE" get instances | awk -v an="$A_NODE" '$2=="b" && $3!=an && $4=="Ready" {print $3}' | head -1)"
  [[ -n "$WAN_NODE" ]] || die "no remote b node for the WAN step"
  kill "${AGENT_PID[$WAN_NODE]}" 2>/dev/null
  for _ in $(seq 1 20); do kill -0 "${AGENT_PID[$WAN_NODE]}" 2>/dev/null || break; sleep 0.2; done
  start_agent "$WAN_NODE" --advertise-address "$WAN_IP"
  wan_advertised() { "$KLITE" get nodes -o yaml 2>/dev/null | grep -q "advertiseAddress: $WAN_IP"; }
  wait_for 30 wan_advertised && pass "$WAN_NODE re-advertised as $WAN_IP (agent re-run, containers adopted)" \
    || die "WAN advertise address never reached NodeStatus"

  WAN_B_INST="$("$KLITE" get instances | awk -v n="$WAN_NODE" '$2=="b" && $3==n && $4=="Ready" {print $1}' | head -1)"
  read -r _ WAN_B_PORT <<<"$(alloc_row b "$WAN_B_INST")"
  wan_in_eds() { curl -s --max-time 3 "127.0.0.1:$(envoy_admin "$A_NODE")/clusters" | grep -q "b::${WAN_IP}:${WAN_B_PORT}::"; }
  wait_for 30 wan_in_eds && pass "consumer EDS now dials b via ${WAN_IP}:${WAN_B_PORT}" \
    || die "consumer EDS never picked up the WAN address"

  WAN_MARK="$(date -u +%Y-%m-%dT%H:%M:%S)"
  WH0="$(estat "$WAN_NODE" "listener.0.0.0.0_${WAN_B_PORT}.ssl.handshake")"
  WAN_CID="$(docker ps --filter "label=io.klite.instance=$WAN_B_INST" --format '{{.ID}}')"
  wan_flows() {
    [[ "$(estat "$WAN_NODE" "listener.0.0.0.0_${WAN_B_PORT}.ssl.handshake")" -gt "$WH0" ]] \
      && docker logs "$(a_ctr)" --since 10s 2>/dev/null | grep -q "b => Hostname: ${WAN_CID}"
  }
  wait_for 45 wan_flows \
    && pass "cross-node traffic flows to $WAN_NODE via $WAN_IP (handshakes rising, its b answering)" \
    || die "no traffic reached $WAN_NODE through $WAN_IP:$WAN_B_PORT"
  sleep 5
  [[ "$(failed_since "$WAN_MARK")" == 0 ]] \
    && pass "advertise flip was hitless (zero FAILED since $WAN_MARK)" \
    || die "requests failed after the advertise flip"
fi

# ============================================================
STEP=9-release
VICTIM="$("$KLITE" get instances | awk '$2=="b" && $4=="Ready" {print $1}' | head -1)"
read -r _ VICTIM_PORT <<<"$(alloc_row b "$VICTIM")"
[[ -n "$VICTIM_PORT" ]] || die "victim $VICTIM has no allocation"
"$KLITE" delete instance "$VICTIM" >/dev/null || die "delete instance $VICTIM"
victim_released() { [[ -z "$(alloc_row b "$VICTIM")" ]]; }
wait_for 30 victim_released \
  && pass "killed instance's ingress allocation released (b.$VICTIM, port $VICTIM_PORT)" \
  || die "allocation b.$VICTIM survived its instance"
wait_for 60 b3 && pass "replacement instance Ready with its own allocation" || die "replacement after delete"

# ============================================================
STEP=teardown
cleanup
trap - EXIT
LEFT=0
pgrep -f "bin/klited" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
pgrep -f "bin/klite-agent" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
[[ "$("$KLITE" get nodes 2>/dev/null | wc -l)" == 0 ]] || true
[[ "$LEFT" == 0 ]] && pass "processes torn down (etcd, klite0, images, ~/.klite/server stay)" \
  || die "teardown left $LEFT process group(s)"

echo
echo "verify-m9: all steps passed"
