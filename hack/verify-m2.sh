#!/usr/bin/env bash
# Checks M2 end to end: three agents run workload b on Docker through klited,
# surviving a container kill, a dead node, and an agent restart. Leaves etcd
# and the klite0 network running when it's done.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
KLITED_PID=""
AGENT1_PID=""
AGENT2_PID=""
AGENT3_PID=""

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }

cleanup() {
  [[ -n "$AGENT1_PID" ]] && kill "$AGENT1_PID" 2>/dev/null
  [[ -n "$AGENT2_PID" ]] && kill "$AGENT2_PID" 2>/dev/null
  [[ -n "$AGENT3_PID" ]] && kill "$AGENT3_PID" 2>/dev/null
  [[ -n "$KLITED_PID" ]] && kill "$KLITED_PID" 2>/dev/null
  docker ps -aq --filter label=io.klite.role=workload | xargs docker rm -f >/dev/null 2>&1
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

# disown keeps bash from announcing the SIGKILL step in the middle of the run.
# TOKEN is minted after klited comes up (M8: agents join with it, or reuse a
# persisted identity from an earlier run).
start_agent() {
  "$BIN/klite-agent" --node "node-$1" --token "$TOKEN" >"/tmp/klite-agent-node-$1.log" 2>&1 &
  disown $!
}

klited_ready()   { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()    { [[ "$("$KLITE" get nodes | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 3 ]]; }
running_b()      { "$KLITE" get instances | awk '$2=="b" && ($4=="Running" || $4=="Ready")'; }
five_running()   { [[ "$(running_b | wc -l | tr -d ' ')" == 5 ]]; }
two_running()    { [[ "$("$KLITE" get instances | awk '$2=="b"' | wc -l | tr -d ' ')" == 2 ]]; }
node3_notready() { "$KLITE" get nodes | awk '$1=="node-3" && $2=="NotReady"' | grep -q node-3; }
node3_ready()    { "$KLITE" get nodes | awk '$1=="node-3" && $2=="Ready"' | grep -q node-3; }
node3_empty()    { [[ "$(docker ps -aq --filter label=io.klite.role=workload --filter label=io.klite.node=node-3 | wc -l | tr -d ' ')" == 0 ]]; }
two_containers() { [[ "$(docker ps -q --filter label=io.klite.workload=b | wc -l | tr -d ' ')" == 2 ]]; }
rescheduled()    { five_running && [[ "$(running_b | awk '$3=="node-3"' | wc -l | tr -d ' ')" == 0 ]]; }
restarted_once() {
  "$KLITE" get instances | awk -v n="$VICTIM_INSTANCE" '$1==n && ($4=="Running" || $4=="Ready") && $5=="1"' | grep -q "$VICTIM_INSTANCE"
}

# --- fresh cluster state ---
hack/etcd-up.sh down >/dev/null 2>&1
# Scoped to the canonical members: other stacks keep their data dirs.
rm -rf "$HOME/.klite/etcd/etcd-1" "$HOME/.klite/etcd/etcd-2" "$HOME/.klite/etcd/etcd-3"
hack/etcd-up.sh >/dev/null 2>&1 \
  && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

go build -o "$BIN/klited" ./cmd/klited \
  && go build -o "$BIN/klite" ./cmd/klite \
  && go build -o "$BIN/klite-agent" ./cmd/klite-agent \
  && pass "build klited, klite, klite-agent" || die "build klited, klite, klite-agent"

"$BIN/klited" --listen 127.0.0.1:7443 >/tmp/klited-7443.log 2>&1 &
KLITED_PID=$!
wait_for 15 klited_ready \
  && pass "klited answering on 7443" || die "klited answering on 7443"
TOKEN="$("$KLITE" node token)" && pass "minted join token" || die "mint join token"

# --- nodes ---
for i in 1 2 3; do
  "$KLITE" apply -f "examples/seed/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "applied 3 node YAMLs"

start_agent 1; AGENT1_PID=$!
start_agent 2; AGENT2_PID=$!
start_agent 3; AGENT3_PID=$!
wait_for 10 nodes_ready \
  && pass "all 3 nodes Ready within 10s" || die "all 3 nodes Ready within 10s"

# --- workload b: run and spread ---
"$KLITE" apply -f examples/seed/apps/b-whoami.yaml >/dev/null \
  && pass "apply b-whoami.yaml" || die "apply b-whoami.yaml"
"$KLITE" scale workload b --replicas 5 >/dev/null \
  && pass "scale workload b to 5" || die "scale workload b to 5"

wait_for 30 five_running \
  && pass "5 instances of b Running" || die "5 instances of b Running"

SPREAD="$(running_b | awk '{print $3}' | sort | uniq -c | awk '{print $1}' | sort -n | xargs)"
[[ "$SPREAD" == "1 2 2" ]] \
  && pass "instances spread 2/2/1 across nodes" || die "instances spread 2/2/1 (got: $SPREAD)"

# --- docker labels ---
[[ "$(docker ps -q --filter label=io.klite.role=workload --filter label=io.klite.workload=b | wc -l | tr -d ' ')" == 5 ]] \
  && pass "docker ps shows 5 labeled b containers" || die "docker ps shows 5 labeled b containers"

CTR="$(docker ps --filter label=io.klite.workload=b --format '{{.Names}}' | head -1)"
LABELS_OK=1
for kv in \
  "io.klite.role workload" \
  "io.klite.workload b"; do
  key="${kv% *}"; want="${kv#* }"
  got="$(docker inspect "$CTR" --format "{{index .Config.Labels \"$key\"}}")"
  [[ "$got" == "$want" ]] || LABELS_OK=0
done
INST_LABEL="$(docker inspect "$CTR" --format '{{index .Config.Labels "io.klite.instance"}}')"
NODE_LABEL="$(docker inspect "$CTR" --format '{{index .Config.Labels "io.klite.node"}}')"
HASH_LABEL="$(docker inspect "$CTR" --format '{{index .Config.Labels "io.klite.template-hash"}}')"
[[ "$LABELS_OK" == 1 && "$CTR" == "klite.$NODE_LABEL.$INST_LABEL" && -n "$HASH_LABEL" ]] \
  && pass "container labels and name match ($CTR)" || die "container labels and name match ($CTR)"

# --- crash restart ---
VICTIM_CTR="$(docker ps --filter label=io.klite.workload=b --format '{{.Names}}' | head -1)"
VICTIM_INSTANCE="$(docker inspect "$VICTIM_CTR" --format '{{index .Config.Labels "io.klite.instance"}}')"
docker kill "$VICTIM_CTR" >/dev/null \
  && pass "docker kill $VICTIM_CTR" || die "docker kill $VICTIM_CTR"
wait_for 10 restarted_once \
  && pass "instance $VICTIM_INSTANCE Running again with RESTARTS 1 within 10s" \
  || die "instance $VICTIM_INSTANCE Running again with RESTARTS 1 within 10s"

# --- dead node: NotReady, then reschedule ---
kill -9 "$AGENT3_PID" 2>/dev/null
AGENT3_PID=""
pass "node-3 agent killed with SIGKILL"
wait_for 20 node3_notready \
  && pass "node-3 NotReady within 20s" || die "node-3 NotReady within 20s"
wait_for 40 rescheduled \
  && pass "instances rescheduled off node-3 within 40s" || die "instances rescheduled off node-3 within 40s"

# --- returning agent cleans up its orphans ---
start_agent 3; AGENT3_PID=$!
wait_for 10 node3_empty \
  && pass "node-3 orphan containers removed within 10s" || die "node-3 orphan containers removed within 10s"
wait_for 10 node3_ready \
  && pass "node-3 Ready again" || die "node-3 Ready again"

# --- scale down ---
# M5: victims drain (default 30s) before deletion when they were READY
# (ADR 0010), so the budget covers the drain timeout plus slack.
"$KLITE" scale workload b --replicas 2 >/dev/null \
  && pass "scale workload b to 2" || die "scale workload b to 2"
wait_for 60 two_running \
  && pass "2 instances remain after scale-down" || die "2 instances remain after scale-down"
wait_for 15 two_containers \
  && pass "2 containers remain in docker" || die "2 containers remain in docker"

echo
echo "verify-m2: all steps passed (etcd and klite0 left running)"
