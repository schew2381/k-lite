#!/usr/bin/env bash
# Checks M4 end to end: per-node infra pods (klite-net + Envoy), DNS + VIPs,
# probe-gated READY, xDS-driven load balancing across nodes, policy check,
# scale-out convergence, and instance-kill recovery. Leaves etcd, the klite0
# network, and images in place.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
KLITED_PID=""
AGENT_PIDS=()

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }

cleanup() {
  for pid in "${AGENT_PIDS[@]:-}"; do [[ -n "$pid" ]] && kill "$pid" 2>/dev/null; done
  [[ -n "$KLITED_PID" ]] && kill "$KLITED_PID" 2>/dev/null
  docker ps -aq --filter label=io.klite.role=workload | xargs docker rm -f >/dev/null 2>&1
  docker ps -aq --filter label=io.klite.role=net | xargs docker rm -f >/dev/null 2>&1
  docker ps -aq --filter label=io.klite.role=envoy | xargs docker rm -f >/dev/null 2>&1
  wait 2>/dev/null
}
trap cleanup EXIT

# wait_for <seconds> <fn>: retries fn every 0.5s within the budget.
wait_for() {
  local budget=$1 fn=$2
  local tries=$((budget * 2))
  for _ in $(seq 1 "$tries"); do
    "$fn" && return 0
    sleep 0.5
  done
  return 1
}

start_agent() {
  "$BIN/klite-agent" --node "node-$1" >"/tmp/klite-m4-agent-node-$1.log" 2>&1 &
  AGENT_PIDS+=($!)
  disown $!
}

klited_ready() { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()  { [[ "$("$KLITE" get nodes | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 3 ]]; }

infra_up() {
  for n in 1 2 3; do
    docker ps --format '{{.Names}}' | grep -qx "klite.node-$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.node-$n.envoy" || return 1
  done
}

# Ready instance counts per workload: a=1, b=2, c=2.
all_ready() {
  [[ "$("$KLITE" get instances | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$("$KLITE" get instances | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]] || return 1
  [[ "$("$KLITE" get instances | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]]
}

b_hostnames_in_tail() { # b_hostnames_in_tail <lines>: distinct b hostnames in a's last <lines> log lines
  "$KLITE" logs "$A_INST" --tail "$1" 2>/dev/null \
    | grep -o 'b => Hostname: [0-9a-f]*' | awk '{print $4}' | sort -u
}

two_b_hostnames()   { [[ "$(b_hostnames_in_tail 30 | wc -l | tr -d ' ')" -ge 2 ]]; }
three_b_hostnames() { [[ "$(b_hostnames_in_tail 30 | wc -l | tr -d ' ')" -ge 3 ]]; }
b_answers_again()   { "$KLITE" logs "$A_INST" --tail 4 2>/dev/null | grep -q 'b => Hostname:'; }

victim_recovered() {
  "$KLITE" get instances | awk -v n="$VICTIM_INST" '$1==n && $4=="Ready" && $5>=1' | grep -q "$VICTIM_INST"
}

# --- fresh cluster state -----------------------------------------------------
pkill -f "$BIN/klited" 2>/dev/null
pkill -f "$BIN/klite-agent" 2>/dev/null
docker ps -aq --filter label=io.klite.role | xargs docker rm -f >/dev/null 2>&1
hack/etcd-up.sh down >/dev/null 2>&1
rm -rf "$HOME/.klite/etcd"
hack/etcd-up.sh >/dev/null 2>&1 \
  && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

make build >/dev/null 2>&1 \
  && pass "make build" || die "make build"
make net-image >/dev/null 2>&1 \
  && pass "make net-image (klite-net:dev)" || die "make net-image"

"$BIN/klited" --listen 127.0.0.1:7443 >/tmp/klite-m4-klited.log 2>&1 &
KLITED_PID=$!
wait_for 15 klited_ready \
  && pass "klited answering on 7443" || die "klited answering on 7443"

# --- nodes and infra pods ----------------------------------------------------
for i in 1 2 3; do
  "$KLITE" apply -f "examples/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "applied 3 node YAMLs"

for i in 1 2 3; do start_agent "$i"; done
wait_for 15 nodes_ready \
  && pass "all 3 nodes Ready" || die "all 3 nodes Ready"
wait_for 60 infra_up \
  && pass "infra pods up on all nodes (klite.<n>.net + klite.<n>.envoy)" \
  || die "infra pods up on all nodes"

# --- workloads a, b, c -------------------------------------------------------
for app in a-client b-whoami c-whoami; do
  "$KLITE" apply -f "examples/apps/$app.yaml" >/dev/null || die "apply $app.yaml"
done
pass "applied a, b, c workloads and services"

wait_for 90 all_ready \
  && pass "all instances READY (a=1, b=2, c=2, probe-gated)" \
  || { "$KLITE" get instances; die "all instances READY"; }

A_INST="$("$KLITE" get instances | awk '$2=="a" {print $1}' | head -1)"
A_NODE="$("$KLITE" get instances | awk '$2=="a" {print $3}' | head -1)"
[[ -n "$A_INST" && -n "$A_NODE" ]] || die "resolve a's instance and node"

# --- load-balanced, cross-node traffic ---------------------------------------
wait_for 45 two_b_hostnames \
  && pass "a's logs alternate between 2 b hostnames within 30 lines" \
  || { "$KLITE" logs "$A_INST" --tail 30; die "a's logs alternate between 2 b hostnames"; }

# whoami's Hostname is its container id (12 hex chars); map ids to nodes.
CROSS=""
while read -r cid cnode; do
  if [[ "$cnode" != "$A_NODE" ]] && b_hostnames_in_tail 30 | grep -q "^${cid}$"; then
    CROSS="$cid@$cnode"
  fi
done < <(docker ps --filter label=io.klite.workload=b --format '{{.ID}} {{.Label "io.klite.node"}}')
[[ -n "$CROSS" ]] \
  && pass "cross-node response observed ($CROSS, a on $A_NODE)" \
  || die "at least one b response from a node other than $A_NODE"

# --- DNS: name resolves to this node's VIP, TTL 5 ----------------------------
A_CTR="klite.$A_NODE.$A_INST"
NSLOOKUP="$(docker exec "$A_CTR" nslookup b.svc.klite 2>&1)"
echo "$NSLOOKUP" | grep -Eq 'Address: *10\.44\.(6[4-9]|[7-9][0-9]|1[0-1][0-9]|12[0-7])\.' \
  && pass "nslookup b.svc.klite in a's container returns a 10.44.64.0/18 VIP" \
  || { echo "$NSLOOKUP"; die "nslookup b.svc.klite returns a VIP"; }

NET_IP="$(docker inspect -f '{{(index .NetworkSettings.Networks "klite0").IPAddress}}' "klite.$A_NODE.net")"
DIG="$(docker run --rm --network klite0 --dns "$NET_IP" alpine:3.20 \
  sh -c "apk add -q --no-cache bind-tools >/dev/null 2>&1 && dig +noall +answer b.svc.klite @$NET_IP" 2>/dev/null)"
echo "$DIG" | grep -Eq 'b\.svc\.klite\.\s+5\s+IN\s+A\s+10\.44\.' \
  && pass "dig shows TTL 5 on b.svc.klite ($(echo "$DIG" | head -1 | tr -s '\t' ' '))" \
  || { echo "$DIG"; die "dig shows TTL 5 on b.svc.klite"; }

# --- Envoy programmed over xDS ------------------------------------------------
# The envoy image ships no curl; /dev/tcp against the shared-netns admin port
# works with plain bash. The admin endpoint requires HTTP/1.1.
DUMP="$(docker exec "klite.$A_NODE.envoy" bash -c \
  'exec 3<>/dev/tcp/127.0.0.1/9901 && printf "GET /config_dump HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n" >&3 && cat <&3' 2>/dev/null)"
echo "$DUMP" | grep -q '"svc/b"' \
  && pass "envoy config_dump on $A_NODE contains the svc/b listener" \
  || die "envoy config_dump contains the svc/b listener"

# --- policy check -------------------------------------------------------------
CHECK="$("$KLITE" policy check a b 2>&1)"
echo "$CHECK" | grep -q 'allowed' \
  && pass "klite policy check a b prints allowed ($CHECK)" \
  || { echo "$CHECK"; die "klite policy check a b prints allowed"; }

# --- scale out: third endpoint appears in traffic ----------------------------
"$KLITE" scale workload b --replicas 3 >/dev/null \
  && pass "scale workload b to 3" || die "scale workload b to 3"
wait_for 30 three_b_hostnames \
  && pass "third b hostname appears in a's logs within 30s" \
  || { b_hostnames_in_tail 30; die "third b hostname appears in a's logs within 30s"; }

# --- kill one b: brief blip, then recovery and restart -----------------------
VICTIM_CTR="$(docker ps --filter label=io.klite.workload=b --format '{{.Names}}' | head -1)"
VICTIM_INST="$(docker inspect "$VICTIM_CTR" --format '{{index .Config.Labels "io.klite.instance"}}')"
docker kill "$VICTIM_CTR" >/dev/null \
  && pass "docker kill $VICTIM_CTR ($VICTIM_INST)" || die "docker kill a b container"
wait_for 45 victim_recovered \
  && pass "killed instance restarted and probed back to Ready (restarts>=1)" \
  || { "$KLITE" get instances; die "killed instance restarts to Ready"; }
sleep 2 # let a's loop run against the recovered set
wait_for 30 b_answers_again \
  && pass "a's loop recovered: fresh b => Hostname lines after the kill" \
  || { "$KLITE" logs "$A_INST" --tail 8; die "a's loop recovers after kill"; }

echo
echo "verify-m4: all steps passed (etcd, klite0, and images left in place)"
