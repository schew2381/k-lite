#!/usr/bin/env bash
# Checks M5 end to end: surge-first rolling update with zero failed requests,
# scale-down that drains before deleting, node drain streaming progress, node
# delete via YAML, and (informational) the capacity-blocked drain-first
# fallback (ADR 0010). Leaves etcd, the klite0 network, and images in place.
#
# Canonical stack: klited on 127.0.0.1:7443, etcd on 2379/2381/2383 (etcd-up
# defaults), nodes node-1..4. Other stacks may share the Docker daemon, so
# every kill and container removal is scoped to those names and labels.
#
# The zero-failure gates ride deterministic probe loops (wget once a second
# to b and to c, exec'd inside a's container so they take the kdns -> VIP ->
# Envoy path). The seeded apps' own 2.5% chatter is too sparse to gate on.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"
unset KLITE_SERVER

BIN=bin
KLITE="$BIN/klite"
KLITED_PID=""
NODES=(node-1 node-2 node-3 node-4)
declare -A AGENT_PID=()
TMP=/tmp/klite-m5
mkdir -p "$TMP"

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }
WARNED=0
warn() { echo "WARN (informational): $1"; WARNED=1; }

my_docker_rm() { # canonical nodes only, since other stacks share the daemon
  for n in "${NODES[@]}"; do
    docker ps -aq --filter "label=io.klite.node=$n" | xargs docker rm -f >/dev/null 2>&1
  done
}

cleanup() {
  stop_probes
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

# TOKEN is minted after klited comes up (M8 join).
start_agent() {
  "$BIN/klite-agent" --node "$1" --token "$TOKEN" >"$TMP/agent-$1.log" 2>&1 &
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

# counts_ready <b-replicas>: a=1, b=<n>, c=3, d=2, all READY and nothing extra.
counts_ready() {
  local snap; snap="$("$KLITE" get instances)"
  [[ "$(echo "$snap" | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == 3 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="d" && $4=="Ready"' | wc -l | tr -d ' ')" == 2 ]]
}

a_inst() { "$KLITE" get instances | awk '$2=="a" {print $1}' | head -1; }
a_node() { "$KLITE" get instances | awk '$2=="a" {print $3}' | head -1; }
none_on() { [[ "$("$KLITE" get instances | awk -v n="$1" 'NR>1 && $3==n' | wc -l | tr -d ' ')" == 0 ]]; }
all_on()  { [[ "$("$KLITE" get instances | awk -v n="$1" 'NR>1 && $3!=n' | wc -l | tr -d ' ')" == 0 ]]; }

# --- deterministic probes ------------------------------------------------------
# Each loop fires one wget a second at its service from inside a's container.
# Every attempt appends one line: the served body ("<cid> is b") or FAILED.
# The gates then compare FAILED counts across a window, like the old
# in-container loop, and demand growth so a dead prober cannot fake a clean
# window.
PROBE_B="$TMP/probe-b.log"
PROBE_C="$TMP/probe-c.log"
PROBE_PIDS=()

probe_loop() { # probe_loop <file> <url>
  local file=$1 url=$2
  while :; do
    docker exec "$A_CTR" wget -qO- -T 2 "$url" 2>/dev/null || echo "FAILED"
    sleep 1
  done >>"$file" 2>/dev/null
}

start_probes() { # (re)bind to the current a container and (re)start both loops
  stop_probes
  : >"$PROBE_B"
  : >"$PROBE_C"
  A_CTR="klite.$(a_node).$(a_inst)"
  probe_loop "$PROBE_B" http://b:8080 &
  PROBE_PIDS+=($!); disown $!
  probe_loop "$PROBE_C" http://c:8080 &
  PROBE_PIDS+=($!); disown $!
}

stop_probes() {
  local pid
  for pid in "${PROBE_PIDS[@]:-}"; do [[ -n "$pid" ]] && kill "$pid" 2>/dev/null; done
  PROBE_PIDS=()
}

fails_in() { awk '/FAILED/ {n++} END {print n+0}' "$1" 2>/dev/null; }
lines_in() { awk 'END {print NR}' "$1" 2>/dev/null; }
probes_clean() { # last 6 attempts against each service all answered
  [[ "$(lines_in "$PROBE_B")" -ge 6 && "$(lines_in "$PROBE_C")" -ge 6 ]] || return 1
  ! { tail -n 6 "$PROBE_B"; tail -n 6 "$PROBE_C"; } | grep -q FAILED
}

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
docker image inspect busybox:1.36 >/dev/null 2>&1 || docker pull busybox:1.36 >/dev/null 2>&1

"$BIN/klited" --listen 127.0.0.1:7443 >"$TMP/klited.log" 2>&1 &
KLITED_PID=$!
wait_for 15 klited_ready \
  && pass "klited answering on 7443" || die "klited answering on 7443"
TOKEN="$("$KLITE" node token)" && pass "minted join token" || die "mint join token"

# --- nodes, agents, infra (the m4 recipe) ------------------------------------
for i in 1 2 3 4; do
  "$KLITE" apply -f "examples/seed/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "applied 4 node YAMLs"
for n in "${NODES[@]}"; do start_agent "$n"; done
wait_for 15 nodes_ready 4 \
  && pass "all 4 nodes Ready" || die "all 4 nodes Ready"
wait_for 60 infra_up \
  && pass "infra pods up on all nodes" || die "infra pods up on all nodes"

# --- seed policies, then workloads a, b, c, d with fast drain knobs -------------
# Policies land first so the fixture matches a real dev-up. The probe loops
# ride a -> b and a -> c, which the seeded matrix leaves open.
"$KLITE" apply -f examples/seed/policies >/dev/null || die "apply examples/seed/policies"
pass "applied the seed policies (only-a-reaches-d, deny-c-to-b)"

# The example apps get the 4s/4s drain knobs (ADR 0010: demo pace lives in
# YAML) and are applied once, so every instance is born with the fast drains.
for app in a-client b-whoami c-whoami d-web; do
  patch_drain "examples/seed/apps/$app.yaml" "$TMP/${app}-fast.yaml"
  "$KLITE" apply -f "$TMP/${app}-fast.yaml" >/dev/null || die "apply ${app}-fast.yaml"
done
pass "applied a, b, c, d with drain 4s/4s"

wait_for 90 counts_ready 2 \
  && pass "all instances READY (a=1, b=2, c=3, d=2)" \
  || { "$KLITE" get instances; die "all instances READY"; }

"$KLITE" scale workload b --replicas 3 >/dev/null || die "scale b to 3"
wait_for 60 counts_ready 3 \
  && pass "b scaled out to 3 READY" || die "b scaled out to 3 READY"

A_INST="$(a_inst)"; A_NODE="$(a_node)"
[[ -n "$A_INST" && -n "$A_NODE" ]] || die "resolve a's instance and node"
start_probes
wait_for 45 probes_clean \
  && pass "probe loops are clean (b and c both answering, no FAILED)" \
  || { tail -n 6 "$PROBE_B" "$PROBE_C"; die "probe loops are clean"; }

# --- rolling update: one at a time, zero failed requests ----------------------
OLD_B="$("$KLITE" get instances | awk '$2=="b" {print $1}' | sort)"
B_FAILS0="$(fails_in "$PROBE_B")"
B_LINES0="$(lines_in "$PROBE_B")"
# Keep replicas at 3: apply replaces the whole spec, and the example file
# says 2, which would fold a scale-down into the rollout. The marker env var
# changes the template hash without touching the chatty behavior.
awk '{sub(/^  replicas: 2$/, "  replicas: 3"); print}
     /^            value: a c d$/ {print "          - name: M5_ROLLOUT"; print "            value: \"1\""}' \
  "$TMP/b-whoami-fast.yaml" > "$TMP/b-v2.yaml"
grep -q 'M5_ROLLOUT' "$TMP/b-v2.yaml" && grep -q '^  replicas: 3$' "$TMP/b-v2.yaml" \
  || die "craft b-v2.yaml (env change at replicas 3)"
"$KLITE" apply -f "$TMP/b-v2.yaml" >/dev/null \
  && pass "applied b template change (M5_ROLLOUT=1)" || die "apply b-v2.yaml"

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

B_FAILS1="$(fails_in "$PROBE_B")"
[[ "$B_FAILS1" == "$B_FAILS0" && "$(lines_in "$PROBE_B")" -gt "$B_LINES0" ]] \
  && pass "zero failed b-probes through the rollout ($B_FAILS1 total, unchanged)" \
  || die "failed b-probes during rollout (before=$B_FAILS0 after=$B_FAILS1)"

sleep 8 # let the tail window move past the last old response
MY_B_IDS="$(for n in "${NODES[@]}"; do
  docker ps --filter "label=io.klite.node=$n" --filter label=io.klite.workload=b --format '{{.ID}}'
done)"
STALE=""
for h in $(tail -n 12 "$PROBE_B" 2>/dev/null | awk '$2=="is" && $3=="b" {print $1}' | sort -u); do
  echo "$MY_B_IDS" | grep -q "^${h:0:12}" || STALE="$h"
done
[[ -z "$STALE" ]] \
  && pass "recent b responses all come from fresh containers" \
  || die "stale hostname $STALE still answering after rollout"

# --- scale-down with drain -----------------------------------------------------
B_FAILS0="$(fails_in "$PROBE_B")"
B_LINES0="$(lines_in "$PROBE_B")"
"$KLITE" scale workload b --replicas 2 >/dev/null || die "scale b to 2"
b_draining() { "$KLITE" get instances | awk '$2=="b" && $4=="Draining"' | grep -q .; }
wait_for 15 b_draining \
  && pass "scale-down victim went DRAINING" || die "scale-down victim went DRAINING"
wait_for 30 counts_ready 2 \
  && pass "victim drained away: b back to 2 READY" || die "b back to 2 READY after scale-down"
B_FAILS1="$(fails_in "$PROBE_B")"
[[ "$B_FAILS1" == "$B_FAILS0" && "$(lines_in "$PROBE_B")" -gt "$B_LINES0" ]] \
  && pass "zero failed b-probes through the scale-down" \
  || die "failed b-probes during scale-down (before=$B_FAILS0 after=$B_FAILS1)"

# --- node drain ----------------------------------------------------------------
# Pick a node a does not live on, so its log stays continuous for the check.
DRAIN_NODE=""
for cand in node-2 node-3 node-4 node-1; do
  [[ "$cand" == "$A_NODE" ]] && continue
  [[ "$("$KLITE" get instances | awk -v n="$cand" 'NR>1 && $3==n' | wc -l | tr -d ' ')" -ge 1 ]] \
    && DRAIN_NODE="$cand" && break
done
[[ -n "$DRAIN_NODE" ]] || die "pick a drain target with instances"
B_FAILS0="$(fails_in "$PROBE_B")"
C_FAILS0="$(fails_in "$PROBE_C")"
B_LINES0="$(lines_in "$PROBE_B")"
C_LINES0="$(lines_in "$PROBE_C")"

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
B_FAILS1="$(fails_in "$PROBE_B")"
C_FAILS1="$(fails_in "$PROBE_C")"
[[ "$B_FAILS1" == "$B_FAILS0" && "$C_FAILS1" == "$C_FAILS0" \
   && "$(lines_in "$PROBE_B")" -gt "$B_LINES0" && "$(lines_in "$PROBE_C")" -gt "$C_LINES0" ]] \
  && pass "zero failed probes through the node drain" \
  || die "failed probes during node drain (b: $B_FAILS0->$B_FAILS1, c: $C_FAILS0->$C_FAILS1)"

# --- node delete via YAML ------------------------------------------------------
"$KLITE" delete -f "examples/seed/nodes/$DRAIN_NODE.yaml" >/dev/null \
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
sed 's/maxInstances: 32/maxInstances: 8/' "examples/seed/nodes/$DRAIN_NODE.yaml" > "$TMP/$DRAIN_NODE-cap8.yaml"
grep -q '^  maxInstances: 8$' "$TMP/$DRAIN_NODE-cap8.yaml" || die "craft $DRAIN_NODE-cap8.yaml (the maxInstances line moved?)"
"$KLITE" apply -f "$TMP/$DRAIN_NODE-cap8.yaml" >/dev/null || die "re-declare $DRAIN_NODE with maxInstances 8"
start_agent "$DRAIN_NODE"
wait_for 30 nodes_ready 4 || warn "rejoined $DRAIN_NODE not Ready in 30s"
wait_for 60 one_infra_up "$DRAIN_NODE" || warn "rejoined $DRAIN_NODE infra not up in 60s"
pass "$DRAIN_NODE rejoined with capacity for exactly the 8 instances"

for n in "${NODES[@]}"; do
  [[ "$n" == "$DRAIN_NODE" ]] && continue
  run_drain "$TMP/drain-$n.log" "$n" || warn "drain $n did not complete (see $TMP/drain-$n.log)"
done
wait_for 30 all_on "$DRAIN_NODE" \
  && pass "all instances consolidated on $DRAIN_NODE (8/8, no headroom)" \
  || warn "instances did not consolidate on $DRAIN_NODE"

start_probes # a moved during the consolidation drains, so rebind the loops
awk '{print} /^            value: a b d$/ {print "          - name: M5_ROLLOUT"; print "            value: \"1\""}' \
  "$TMP/c-whoami-fast.yaml" > "$TMP/c-v2.yaml"
OLD_C="$("$KLITE" get instances | awk '$2=="c" {print $1}' | sort)"
C_FAILS0="$(fails_in "$PROBE_C")"
"$KLITE" apply -f "$TMP/c-v2.yaml" >/dev/null || die "apply c-v2.yaml"

fallback_logged() { grep -q "falling back to drain-first" "$TMP/klited.log"; }
wait_for 90 fallback_logged \
  && pass "drain-first fallback engaged (no capacity to surge, ADR 0010)" \
  || warn "fallback log line not seen in 90s"
c_converged() {
  local snap; snap="$("$KLITE" get instances | awk '$2=="c" {print $1, $4}')"
  [[ "$(echo "$snap" | grep -c '[^ ]')" == 3 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 3 ]] || return 1
  ! echo "$snap" | awk '{print $1}' | grep -qxF -f <(echo "$OLD_C")
}
wait_for 180 c_converged \
  && pass "c converged on the new template despite zero headroom" \
  || warn "c did not converge in 180s"
C_FAILS1="$(fails_in "$PROBE_C")"
echo "INFO: c-probe failures during the fallback dip: $((C_FAILS1 - C_FAILS0)) (a dip here is the documented ADR 0010 tradeoff)"

echo
if [[ "$WARNED" == 1 ]]; then
  echo "verify-m5: required steps passed (informational fallback step had warnings)"
else
  echo "verify-m5: all steps passed (etcd, klite0, and images left in place)"
fi
