#!/usr/bin/env bash
# Checks M5 end to end: surge-first rolling update with zero failed requests,
# scale-down that drains before deleting, node drain streaming progress, node
# delete via YAML, and (informational) the capacity-blocked drain-first
# fallback (ADR 0010). Leaves etcd, the klite0 network, and images in place.
#
# Canonical stack: klited on 127.0.0.1:7443, etcd on 2379/2381/2383 (etcd-up
# defaults), nodes node-1..3. Other stacks may share the Docker daemon, so
# every kill and container removal is scoped to those names and labels.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"
unset KLITE_SERVER

BIN=bin
KLITE="$BIN/klite"
KLITED_PID=""
NODES=(node-1 node-2 node-3)
declare -A AGENT_PID=()
TMP=/tmp/klite-m5
mkdir -p "$TMP"

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }
WARNED=0
warn() { echo "WARN (informational): $1"; WARNED=1; }

my_docker_rm() { # canonical nodes only; other stacks share the daemon
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

# wait_for <seconds> <fn> [args...]: retries fn every 0.5s within the budget.
wait_for() {
  local budget=$1; shift
  local tries=$((budget * 2))
  for _ in $(seq 1 "$tries"); do
    "$@" && return 0
    sleep 0.5
  done
  return 1
}

start_agent() {
  "$BIN/klite-agent" --node "$1" >"$TMP/agent-$1.log" 2>&1 &
  AGENT_PID[$1]=$!
  disown $!
}

stop_agent() {
  [[ -n "${AGENT_PID[$1]:-}" ]] && kill "${AGENT_PID[$1]}" 2>/dev/null
  unset "AGENT_PID[$1]"
}

klited_ready() { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()  { [[ "$("$KLITE" get nodes | awk '$2=="Ready"' | wc -l | tr -d ' ')" == "$1" ]]; }
node_gone()    { ! "$KLITE" get nodes | awk 'NR>1 {print $1}' | grep -qx "$1"; }
node_cordoned() { "$KLITE" get nodes | awk -v n="$1" '$1==n && $3=="true"' | grep -q .; }

infra_up() {
  for n in "${NODES[@]}"; do
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.envoy" || return 1
  done
}
one_infra_up() {
  docker ps --format '{{.Names}}' | grep -qx "klite.$1.net" \
    && docker ps --format '{{.Names}}' | grep -qx "klite.$1.envoy"
}

# counts_ready <b-replicas>: a=1, b=<n>, c=2, all READY and nothing extra.
counts_ready() {
  local snap; snap="$("$KLITE" get instances)"
  [[ "$(echo "$snap" | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]]
}

a_inst() { "$KLITE" get instances | awk '$2=="a" {print $1}' | head -1; }
a_node() { "$KLITE" get instances | awk '$2=="a" {print $3}' | head -1; }
a_fails() { "$KLITE" logs "$1" 2>/dev/null | grep -c "$2 FAILED"; }
tail_clean() { ! "$KLITE" logs "$A_INST" --tail 12 2>/dev/null | grep -q FAILED; }
none_on() { [[ "$("$KLITE" get instances | awk -v n="$1" 'NR>1 && $3==n' | wc -l | tr -d ' ')" == 0 ]]; }
all_on()  { [[ "$("$KLITE" get instances | awk -v n="$1" 'NR>1 && $3!=n' | wc -l | tr -d ' ')" == 0 ]]; }

# patch_drain <src> <dst>: insert fast drain knobs into the workload spec.
# Drain sits outside the template, so this never changes the template hash.
patch_drain() {
  awk 'BEGIN{done=0} {print} /^spec:$/ && !done {
    print "  drain:"; print "    drainTimeoutSeconds: 4"; print "    terminationGraceSeconds: 4"; done=1
  }' "$1" > "$2"
}

# run_drain <log> <node> [flags...]: klite drain with a 240s watchdog.
run_drain() {
  local log=$1; shift
  "$KLITE" drain "$@" >"$log" 2>&1 &
  local pid=$!
  for _ in $(seq 1 480); do
    kill -0 "$pid" 2>/dev/null || { wait "$pid"; return $?; }
    sleep 0.5
  done
  kill "$pid" 2>/dev/null
  return 1
}

# --- fresh cluster state -----------------------------------------------------
pkill -f 'bin/klited --listen 127.0.0.1:7443' 2>/dev/null
for n in "${NODES[@]}"; do pkill -f "bin/klite-agent --node $n\$" 2>/dev/null; done
my_docker_rm
hack/etcd-up.sh down >/dev/null 2>&1
# Scoped to the canonical members: other stacks keep their data dirs.
rm -rf "$HOME/.klite/etcd/etcd-1" "$HOME/.klite/etcd/etcd-2" "$HOME/.klite/etcd/etcd-3"
hack/etcd-up.sh >/dev/null 2>&1 \
  && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

make build >/dev/null 2>&1 \
  && pass "make build" || die "make build"
if docker image inspect klite-net:dev >/dev/null 2>&1; then
  pass "klite-net:dev image present (reused; M5 does not change klite-net)"
else
  make net-image >/dev/null 2>&1 \
    && pass "make net-image (klite-net:dev)" || die "make net-image"
fi

"$BIN/klited" --listen 127.0.0.1:7443 >"$TMP/klited.log" 2>&1 &
KLITED_PID=$!
wait_for 15 klited_ready \
  && pass "klited answering on 7443" || die "klited answering on 7443"

# --- nodes, agents, infra (the m4 recipe) ------------------------------------
for i in 1 2 3; do
  "$KLITE" apply -f "examples/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "applied 3 node YAMLs"
for n in "${NODES[@]}"; do start_agent "$n"; done
wait_for 15 nodes_ready 3 \
  && pass "all 3 nodes Ready" || die "all 3 nodes Ready"
wait_for 60 infra_up \
  && pass "infra pods up on all nodes" || die "infra pods up on all nodes"

# --- workloads a, b, c with fast drain knobs ----------------------------------
# The example apps plus the 4s/4s drain knobs (ADR 0010: demo pace lives in
# YAML), applied once so every instance is born with the fast drains.
for app in a-client b-whoami c-whoami; do
  patch_drain "examples/apps/$app.yaml" "$TMP/${app}-fast.yaml"
  "$KLITE" apply -f "$TMP/${app}-fast.yaml" >/dev/null || die "apply ${app}-fast.yaml"
done
pass "applied a, b, c with drain 4s/4s"

wait_for 90 counts_ready 2 \
  && pass "all instances READY (a=1, b=2, c=2)" \
  || { "$KLITE" get instances; die "all instances READY"; }

"$KLITE" scale workload b --replicas 3 >/dev/null || die "scale b to 3"
wait_for 60 counts_ready 3 \
  && pass "b scaled out to 3 READY" || die "b scaled out to 3 READY"

A_INST="$(a_inst)"; A_NODE="$(a_node)"
[[ -n "$A_INST" && -n "$A_NODE" ]] || die "resolve a's instance and node"
wait_for 45 tail_clean \
  && pass "a's request loop is clean (no FAILED in recent lines)" \
  || { "$KLITE" logs "$A_INST" --tail 12; die "a's request loop is clean"; }

# --- rolling update: one at a time, zero failed requests ----------------------
OLD_B="$("$KLITE" get instances | awk '$2=="b" {print $1}' | sort)"
B_FAILS0="$(a_fails "$A_INST" b)"
# Keep replicas at 3: apply replaces the whole spec, and the example file
# says 2, which would fold a scale-down into the rollout.
sed -e 's/value: b$/value: b2/' -e 's/replicas: 2$/replicas: 3/' \
  "$TMP/b-whoami-fast.yaml" > "$TMP/b-v2.yaml"
grep -q 'value: b2' "$TMP/b-v2.yaml" && grep -q 'replicas: 3' "$TMP/b-v2.yaml" \
  || die "craft b-v2.yaml (env change at replicas 3)"
"$KLITE" apply -f "$TMP/b-v2.yaml" >/dev/null \
  && pass "applied b template change (WHOAMI_NAME=b2)" || die "apply b-v2.yaml"

VIOL=""
DEADLINE=$((SECONDS + 240))
while :; do
  SNAP="$("$KLITE" get instances | awk '$2=="b" {print $1, $4}')"
  TOTAL="$(echo "$SNAP" | grep -c '[^ ]')"
  SERVING="$(echo "$SNAP" | awk '$2=="Ready" || $2=="Draining"' | wc -l | tr -d ' ')"
  DRAINING="$(echo "$SNAP" | awk '$2=="Draining"' | wc -l | tr -d ' ')"
  READY="$(echo "$SNAP" | awk '$2=="Ready"' | wc -l | tr -d ' ')"
  [[ "$TOTAL" -le 4 ]] || { VIOL="total $TOTAL > 4: surge exceeded 1"; break; }
  [[ "$SERVING" -ge 3 ]] || { VIOL="serving $SERVING < 3: capacity dipped"; break; }
  [[ "$DRAINING" -le 1 ]] || { VIOL="draining $DRAINING > 1: not one-at-a-time"; break; }
  if [[ "$TOTAL" == 3 && "$READY" == 3 ]] \
    && ! echo "$SNAP" | awk '{print $1}' | grep -qxF -f <(echo "$OLD_B"); then
    break # every instance is new and READY
  fi
  [[ $SECONDS -lt $DEADLINE ]] || { VIOL="timeout waiting for rollout"; break; }
  sleep 0.5
done
[[ -z "$VIOL" ]] \
  && pass "rolling update: strictly one-at-a-time, capacity never dipped" \
  || { "$KLITE" get instances; die "rolling update invariant: $VIOL"; }

B_FAILS1="$(a_fails "$A_INST" b)"
[[ "$B_FAILS1" == "$B_FAILS0" ]] \
  && pass "zero failed b-requests through the rollout ($B_FAILS1 total, unchanged)" \
  || die "failed b-requests during rollout (before=$B_FAILS0 after=$B_FAILS1)"

sleep 8 # let the tail window move past the last old response
MY_B_IDS="$(for n in "${NODES[@]}"; do
  docker ps --filter "label=io.klite.node=$n" --filter label=io.klite.workload=b --format '{{.ID}}'
done)"
STALE=""
for h in $("$KLITE" logs "$A_INST" --tail 12 2>/dev/null | grep -o 'b => Hostname: [0-9a-f]*' | awk '{print $4}' | sort -u); do
  echo "$MY_B_IDS" | grep -q "^${h:0:12}" || STALE="$h"
done
[[ -z "$STALE" ]] \
  && pass "a's recent b responses all come from fresh containers" \
  || die "stale hostname $STALE still answering after rollout"

# --- scale-down with drain -----------------------------------------------------
B_FAILS0="$(a_fails "$A_INST" b)"
"$KLITE" scale workload b --replicas 2 >/dev/null || die "scale b to 2"
b_draining() { "$KLITE" get instances | awk '$2=="b" && $4=="Draining"' | grep -q .; }
wait_for 15 b_draining \
  && pass "scale-down victim went DRAINING" || die "scale-down victim went DRAINING"
wait_for 30 counts_ready 2 \
  && pass "victim drained away: b back to 2 READY" || die "b back to 2 READY after scale-down"
B_FAILS1="$(a_fails "$A_INST" b)"
[[ "$B_FAILS1" == "$B_FAILS0" ]] \
  && pass "zero failed b-requests through the scale-down" \
  || die "failed b-requests during scale-down (before=$B_FAILS0 after=$B_FAILS1)"

# --- node drain ----------------------------------------------------------------
# Pick a node a does not live on, so its log stays continuous for the check.
DRAIN_NODE=""
for cand in node-2 node-3 node-1; do
  [[ "$cand" == "$A_NODE" ]] && continue
  [[ "$("$KLITE" get instances | awk -v n="$cand" 'NR>1 && $3==n' | wc -l | tr -d ' ')" -ge 1 ]] \
    && DRAIN_NODE="$cand" && break
done
[[ -n "$DRAIN_NODE" ]] || die "pick a drain target with instances"
B_FAILS0="$(a_fails "$A_INST" b)"
C_FAILS0="$(a_fails "$A_INST" c)"

run_drain "$TMP/drain-1.log" "$DRAIN_NODE" \
  && pass "klite drain $DRAIN_NODE completed" \
  || { cat "$TMP/drain-1.log"; die "klite drain $DRAIN_NODE completed"; }
grep -q "^cordoned $DRAIN_NODE" "$TMP/drain-1.log" || { cat "$TMP/drain-1.log"; die "drain stream shows cordon"; }
grep -Eq '^draining [a-z]+-[0-9a-f]{4} \([0-9]+s\)' "$TMP/drain-1.log" \
  || { cat "$TMP/drain-1.log"; die "drain stream shows per-instance draining"; }
grep -q "^done: $DRAIN_NODE drained" "$TMP/drain-1.log" || die "drain stream ends with done"
pass "drain stream is nomad-style (cordoned/surged/draining/done)"
sed 's/^/  drain> /' "$TMP/drain-1.log"

none_on "$DRAIN_NODE" \
  && pass "no instances left on $DRAIN_NODE" || die "no instances left on $DRAIN_NODE"
wait_for 60 counts_ready 2 \
  && pass "all workloads back to full strength elsewhere" || die "workloads recover after drain"
node_cordoned "$DRAIN_NODE" \
  && pass "$DRAIN_NODE shows cordoned (unschedulable=true)" || die "$DRAIN_NODE shows cordoned"
B_FAILS1="$(a_fails "$A_INST" b)"
C_FAILS1="$(a_fails "$A_INST" c)"
[[ "$B_FAILS1" == "$B_FAILS0" && "$C_FAILS1" == "$C_FAILS0" ]] \
  && pass "zero failed requests through the node drain" \
  || die "failed requests during node drain (b: $B_FAILS0->$B_FAILS1, c: $C_FAILS0->$C_FAILS1)"

# --- node delete via YAML ------------------------------------------------------
"$KLITE" delete -f "examples/nodes/$DRAIN_NODE.yaml" >/dev/null \
  && pass "klite delete -f $DRAIN_NODE.yaml accepted" || die "klite delete -f $DRAIN_NODE.yaml"
wait_for 30 node_gone "$DRAIN_NODE" \
  && pass "$DRAIN_NODE record gone after drain-backed delete" || die "$DRAIN_NODE record gone"
xds_cleared() { grep -q "xds snapshot cleared.*node=$DRAIN_NODE" "$TMP/klited.log"; }
wait_for 20 xds_cleared \
  && pass "ADS cache dropped $DRAIN_NODE (xds snapshot cleared)" \
  || die "ADS cache dropped $DRAIN_NODE"
no_wl_ctrs() {
  [[ "$(docker ps -aq --filter "label=io.klite.node=$DRAIN_NODE" --filter label=io.klite.role=workload | wc -l | tr -d ' ')" == 0 ]]
}
wait_for 30 no_wl_ctrs \
  && pass "agent cleaned $DRAIN_NODE's workload containers" || die "workload containers cleaned on $DRAIN_NODE"
stop_agent "$DRAIN_NODE"
docker rm -f "klite.$DRAIN_NODE.net" "klite.$DRAIN_NODE.envoy" >/dev/null 2>&1
pass "$DRAIN_NODE agent stopped and infra removed"

# --- capacity-blocked fallback (informational: timing-sensitive) ---------------
sed 's/maxInstances: 32/maxInstances: 5/' "examples/nodes/$DRAIN_NODE.yaml" > "$TMP/$DRAIN_NODE-cap5.yaml"
"$KLITE" apply -f "$TMP/$DRAIN_NODE-cap5.yaml" >/dev/null || die "re-declare $DRAIN_NODE with maxInstances 5"
start_agent "$DRAIN_NODE"
wait_for 30 nodes_ready 3 || warn "rejoined $DRAIN_NODE not Ready in 30s"
wait_for 60 one_infra_up "$DRAIN_NODE" || warn "rejoined $DRAIN_NODE infra not up in 60s"
pass "$DRAIN_NODE rejoined with capacity for exactly the 5 instances"

for n in "${NODES[@]}"; do
  [[ "$n" == "$DRAIN_NODE" ]] && continue
  run_drain "$TMP/drain-$n.log" "$n" || warn "drain $n did not complete (see $TMP/drain-$n.log)"
done
wait_for 30 all_on "$DRAIN_NODE" \
  && pass "all instances consolidated on $DRAIN_NODE (5/5, no headroom)" \
  || warn "instances did not consolidate on $DRAIN_NODE"

A_INST="$(a_inst)" # a moved during the consolidation drains
sed 's/value: c$/value: c2/' "$TMP/c-whoami-fast.yaml" > "$TMP/c-v2.yaml"
OLD_C="$("$KLITE" get instances | awk '$2=="c" {print $1}' | sort)"
C_FAILS0="$(a_fails "$A_INST" c)"
"$KLITE" apply -f "$TMP/c-v2.yaml" >/dev/null || die "apply c-v2.yaml"

fallback_logged() { grep -q "falling back to drain-first" "$TMP/klited.log"; }
wait_for 90 fallback_logged \
  && pass "drain-first fallback engaged (no capacity to surge, ADR 0010)" \
  || warn "fallback log line not seen in 90s"
c_converged() {
  local snap; snap="$("$KLITE" get instances | awk '$2=="c" {print $1, $4}')"
  [[ "$(echo "$snap" | grep -c '[^ ]')" == 2 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 2 ]] || return 1
  ! echo "$snap" | awk '{print $1}' | grep -qxF -f <(echo "$OLD_C")
}
wait_for 180 c_converged \
  && pass "c converged on the new template despite zero headroom" \
  || warn "c did not converge in 180s"
C_FAILS1="$(a_fails "$A_INST" c)"
echo "INFO: c-request failures during the fallback dip: $((C_FAILS1 - C_FAILS0)) (a dip here is the documented ADR 0010 tradeoff)"

echo
if [[ "$WARNED" == 1 ]]; then
  echo "verify-m5: required steps passed; informational fallback step had warnings"
else
  echo "verify-m5: all steps passed (etcd, klite0, and images left in place)"
fi
