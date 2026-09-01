#!/usr/bin/env bash
# Checks M4 end to end. In run order:
#   infra      per-node infra pods (klite-net + Envoy)
#   discovery  DNS answers with VIPs, probe-gated READY
#   balancing  xDS-driven load balancing across nodes
#   policy     klite policy check
#   churn      scale-out convergence, instance-kill recovery
#   chatter    the seeded apps' own random calls complete
# Traffic assertions ride a deterministic probe loop (wget once a second from
# inside a's container). The apps' 2.5% random chatter is checked last, on its
# own clock.
# Leaves etcd, the klite0 network, and images in place.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
KLITED_PID=""
AGENT_PIDS=()
PROBE_PID=""
PROBE_FILE=/tmp/klite-m4-probe.log

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }

cleanup() {
  [[ -n "$PROBE_PID" ]] && kill "$PROBE_PID" 2>/dev/null
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

# TOKEN is minted after klited comes up (M8 join).
start_agent() {
  "$BIN/klite-agent" --node "node-$1" --token "$TOKEN" >"/tmp/klite-m4-agent-node-$1.log" 2>&1 &
  AGENT_PIDS+=($!)
  disown $!
}

klited_ready() { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()  { [[ "$("$KLITE" get nodes | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 4 ]]; }

infra_up() {
  for n in 1 2 3 4; do
    docker ps --format '{{.Names}}' | grep -qx "klite.node-$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.node-$n.envoy" || return 1
  done
}

# Ready instance counts per workload: a=1, b=2, c=3, d=2.
all_ready() {
  [[ "$("$KLITE" get instances | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$("$KLITE" get instances | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]] || return 1
  [[ "$("$KLITE" get instances | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == 3 ]] || return 1
  [[ "$("$KLITE" get instances | awk '$2=="d" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]]
}

# The seeded apps chat at random (a 2.5% roll per second, see examples/seed/apps),
# which is the wrong clock to hang assertions on. The script runs its own
# probe loop instead: one wget per second to b's service, executed inside
# a's container so it rides the same kdns -> VIP -> Envoy path. Each probe
# appends one line to PROBE_FILE: the served body ("<cid> is b") or FAILED.
probe_loop() {
  while :; do
    docker exec "$A_CTR" wget -qO- -T 2 http://b:8080 2>/dev/null || echo "FAILED"
    sleep 1
  done >>"$PROBE_FILE" 2>/dev/null
}

b_hostnames_in_tail() { # b_hostnames_in_tail <lines>: distinct b endpoints in the last <lines> probes
  tail -n "$1" "$PROBE_FILE" 2>/dev/null | awk '$2=="is" && $3=="b" {print $1}' | sort -u
}

two_b_hostnames()   { [[ "$(b_hostnames_in_tail 30 | wc -l | tr -d ' ')" -ge 2 ]]; }
three_b_hostnames() { [[ "$(b_hostnames_in_tail 30 | wc -l | tr -d ' ')" -ge 3 ]]; }
b_answers_again()   { tail -n 4 "$PROBE_FILE" 2>/dev/null | grep -q 'is b'; }

victim_recovered() {
  "$KLITE" get instances | awk -v n="$VICTIM_INST" '$1==n && $4=="Ready" && $5>=1' | grep -q "$VICTIM_INST"
}

# --- fresh cluster state -----------------------------------------------------
pkill -f "$BIN/klited" 2>/dev/null
pkill -f "$BIN/klite-agent" 2>/dev/null
docker ps -aq --filter label=io.klite.role | xargs docker rm -f >/dev/null 2>&1
hack/etcd-up.sh down >/dev/null 2>&1
# Scoped to the canonical members: other stacks keep their data dirs.
rm -rf "$HOME/.klite/etcd/etcd-1" "$HOME/.klite/etcd/etcd-2" "$HOME/.klite/etcd/etcd-3"
hack/etcd-up.sh >/dev/null 2>&1 \
  && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

make build >/dev/null 2>&1 \
  && pass "make build" || die "make build"
make net-image >/dev/null 2>&1 \
  && pass "make net-image (klite-net:dev)" || die "make net-image"
docker image inspect busybox:1.36 >/dev/null 2>&1 || docker pull busybox:1.36 >/dev/null 2>&1

"$BIN/klited" --listen 127.0.0.1:7443 >/tmp/klite-m4-klited.log 2>&1 &
KLITED_PID=$!
wait_for 15 klited_ready \
  && pass "klited answering on 7443" || die "klited answering on 7443"
TOKEN="$("$KLITE" node token)" && pass "minted join token" || die "mint join token"

# --- nodes and infra pods ----------------------------------------------------
for i in 1 2 3 4; do
  "$KLITE" apply -f "examples/seed/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "applied 4 node YAMLs"

for i in 1 2 3 4; do start_agent "$i"; done
wait_for 15 nodes_ready \
  && pass "all 4 nodes Ready" || die "all 4 nodes Ready"
wait_for 60 infra_up \
  && pass "infra pods up on all nodes (klite.<n>.net + klite.<n>.envoy)" \
  || die "infra pods up on all nodes"

# --- workloads a, b, c, d ------------------------------------------------------
for app in a-client b-whoami c-whoami d-web; do
  "$KLITE" apply -f "examples/seed/apps/$app.yaml" >/dev/null || die "apply $app.yaml"
done
pass "applied a, b, c, d workloads and services"

wait_for 90 all_ready \
  && pass "all instances READY (a=1, b=2, c=3, d=2, probe-gated)" \
  || { "$KLITE" get instances; die "all instances READY"; }

A_INST="$("$KLITE" get instances | awk '$2=="a" {print $1}' | head -1)"
A_NODE="$("$KLITE" get instances | awk '$2=="a" {print $3}' | head -1)"
[[ -n "$A_INST" && -n "$A_NODE" ]] || die "resolve a's instance and node"
A_CTR="klite.$A_NODE.$A_INST"

: >"$PROBE_FILE"
probe_loop &
PROBE_PID=$!
disown $!

# --- load-balanced, cross-node traffic ---------------------------------------
wait_for 45 two_b_hostnames \
  && pass "probes from a's netns reach 2 distinct b endpoints within 30 requests" \
  || { tail -n 30 "$PROBE_FILE"; die "probes reach 2 distinct b endpoints"; }

# The served page reads "<hostname> is b", and a container's default hostname
# is its id (12 hex chars), so map ids to nodes.
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
# The envoy image ships no curl, so /dev/tcp against the shared-netns admin
# port does the job in plain bash. The admin endpoint requires HTTP/1.1.
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
  && pass "third b endpoint appears in the probe stream within 30s" \
  || { b_hostnames_in_tail 30; die "third b endpoint appears in the probe stream within 30s"; }

# --- kill one b: brief blip, then recovery and restart -----------------------
VICTIM_CTR="$(docker ps --filter label=io.klite.workload=b --format '{{.Names}}' | head -1)"
VICTIM_INST="$(docker inspect "$VICTIM_CTR" --format '{{index .Config.Labels "io.klite.instance"}}')"
docker kill "$VICTIM_CTR" >/dev/null \
  && pass "docker kill $VICTIM_CTR ($VICTIM_INST)" || die "docker kill a b container"
wait_for 45 victim_recovered \
  && pass "killed instance restarted and probed back to Ready (restarts>=1)" \
  || { "$KLITE" get instances; die "killed instance restarts to Ready"; }
sleep 2 # let the probe loop run against the recovered set
wait_for 30 b_answers_again \
  && pass "probes recovered: fresh 'is b' responses after the kill" \
  || { tail -n 8 "$PROBE_FILE"; die "probes recover after kill"; }

# --- the seeded chatter itself ------------------------------------------------
# The apps also call each other on their own 2.5% roll. By this point the run
# is minutes old, so every workload should have landed at least one call.
chatty_flowing() {
  local wl
  for wl in a b c d; do
    docker ps --filter "label=io.klite.workload=$wl" --format '{{.Names}}' \
      | xargs -I{} docker logs {} 2>/dev/null | grep -q '^-> [abcd] ok$' || return 1
  done
}
wait_for 180 chatty_flowing \
  && pass "chatty traffic flows: a, b, c, and d each logged a '-> <target> ok' call" \
  || die "a workload (a, b, c, or d) never completed a chatty call"

echo
echo "verify-m4: all steps passed (etcd, klite0, and images left in place)"
