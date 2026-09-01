#!/usr/bin/env bash
# Checks M7 end to end: HA chaos against a fully isolated stack so it can run
# next to the canonical dev cluster (klited :7443, etcd-1..3 on :2379/81/83).
#
# This stack, and only this stack, is what the script creates and destroys:
#   klited replicas   127.0.0.1:9443 and 127.0.0.1:9445
#   etcd members      etcd-m7-1..3 on 127.0.0.1:4379/4381/4383, network klite-etcd-m7
#   nodes             m7-1, m7-2, m7-3 (agent processes)
#   workload          m7-web (traefik/whoami x4)
# hack/etcd-up.sh has no port/name overrides, so the etcd trio is run inline
# here with the same flags under m7 names.
#
# Steps: boot, baseline, kill-the-leader-during-scale-churn, CLI failover,
# restart the dead klited, etcd member down (plus an informational quorum-loss
# probe), agent kill and reschedule, rollout-resume-under-leader-kill (tree
# builds only; the pinned ref predates M5 rollouts), and teardown.
# Exits nonzero on the first gating failure. KEEP_M7=1 skips teardown on exit.
set -u

cd "$(dirname "$0")/.."
REPO="$(pwd)"
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

WORK=/tmp/klite-m7
BIN="$WORK/bin"
KLITE="$BIN/klite"
EP_A=127.0.0.1:9443
EP_B=127.0.0.1:9445
BOTH="$EP_A,$EP_B"
ETCD_EPS="127.0.0.1:4379,127.0.0.1:4381,127.0.0.1:4383"
ETCD_IMG="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
ETCD_NET=klite-etcd-m7
NODES="m7-1 m7-2 m7-3"
WL=m7-web
LOG_A="$WORK/klited-9443.log"
LOG_B="$WORK/klited-9445.log"

KLITED_A_PID=""
KLITED_B_PID=""
CHURN_PID=""
TOKEN=""
STEP=prep

pass() { echo "PASS [$STEP]: $1"; }
info() { echo "INFO [$STEP]: $1"; }
die()  { echo "FAIL [$STEP]: $1"; echo "logs under $WORK"; exit 1; }

t_ms() { perl -MTime::HiRes=time -e 'printf("%d\n", time()*1000)'; }

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

# ---------- teardown: strictly m7-scoped, idempotent ----------
teardown() {
  local f n ids
  for f in "$WORK"/agent-m7-*.pid; do
    [ -f "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null
  done
  [ -n "$CHURN_PID" ] && kill "$CHURN_PID" 2>/dev/null
  [ -n "$KLITED_A_PID" ] && kill -9 "$KLITED_A_PID" 2>/dev/null
  [ -n "$KLITED_B_PID" ] && kill -9 "$KLITED_B_PID" 2>/dev/null
  pkill -9 -f "$BIN/" 2>/dev/null # backstop: only processes launched from the m7 bin dir
  for n in $NODES; do
    ids=$(docker ps -aq --filter "label=io.klite.node=$n")
    [ -n "$ids" ] && echo "$ids" | xargs docker rm -f >/dev/null 2>&1
  done
  # Containers are named klite.<node>.<x>, so this catches m7 leftovers that
  # predate labels or carry other roles (infra pods) without touching node-*.
  ids=$(docker ps -aq --filter "name=klite.m7-")
  [ -n "$ids" ] && echo "$ids" | xargs docker rm -f >/dev/null 2>&1
  docker rm -f etcd-m7-1 etcd-m7-2 etcd-m7-3 >/dev/null 2>&1
  docker network rm "$ETCD_NET" >/dev/null 2>&1
  rm -rf "$WORK/etcd" 2>/dev/null # legacy data dirs from older harness versions
  return 0
}
on_exit() {
  if [ "${KEEP_M7:-0}" = 1 ]; then
    echo "KEEP_M7=1: leaving the m7 stack running"
  else
    teardown
  fi
}
trap on_exit EXIT

# ---------- cluster read helpers (always through the CLI) ----------
nodes_ready_count() {
  "$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' '
}
all_nodes_ready() { [ "$(nodes_ready_count)" = 3 ]; }
node_not_ready() {
  "$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk -v n="$1" '$1==n && $2=="NotReady"' | grep -q .
}
# Running or Ready both count: M4 renamed the steady phase for probed instances.
running_count() {
  "$KLITE" --server "$BOTH" get instances 2>/dev/null | awk -v wl="$WL" '$2==wl && ($4=="Running" || $4=="Ready")' | wc -l | tr -d ' '
}
# node/name pairs, one per line, sorted: the store's view and Docker's view.
instances_pairs() {
  "$KLITE" --server "$BOTH" get instances 2>/dev/null | awk -v wl="$WL" '$2==wl {print $3 "/" $1}' | sort
}
container_pairs() {
  local n
  for n in $NODES; do
    docker ps -a --filter "label=io.klite.role=workload" --filter "label=io.klite.node=$n" \
      --format '{{.Label "io.klite.node"}}/{{.Label "io.klite.instance"}}'
  done | sort
}
dup_instances() { container_pairs | awk -F/ '{print $2}' | sort | uniq -d; }
# converged <n>: n instances Running, containers match instance objects
# one-to-one (no orphans, no duplicates).
converged() {
  local want=$1 inst ctrs
  [ "$(running_count)" = "$want" ] || return 1
  inst="$(instances_pairs)"
  ctrs="$(container_pairs)"
  [ "$(printf '%s' "$inst" | grep -c /)" = "$want" ] || return 1
  [ "$inst" = "$ctrs" ] || return 1
  [ -z "$(dup_instances)" ]
}
converged_running() { converged "$1"; }

# last leadership marker in a klited log: leading | standby | none
leader_state() {
  local line
  line=$(grep -E "controllers: (leading|standing by|leadership released)" "$1" 2>/dev/null | tail -1)
  case "$line" in
    *"controllers: leading"*) echo leading ;;
    *"controllers: standing by"*) echo standby ;;
    *) echo none ;;
  esac
}

# M8 trees join with a token and fail over across both klited replicas; the
# pinned pre-M8 ref knows neither flag nor endpoint lists, so agents pin to
# the standby the harness never kills.
start_agent() {
  if [ "$TREE_BUILD" = 1 ]; then
    "$BIN/klite-agent" --node "$1" --server "$BOTH" --token "$TOKEN" >"$WORK/agent-$1.log" 2>&1 &
  else
    "$BIN/klite-agent" --node "$1" --server "$EP_B" >"$WORK/agent-$1.log" 2>&1 &
  fi
  echo $! >"$WORK/agent-$1.pid"
  disown
}

etcd_healthy() { docker exec etcd-m7-1 etcdctl endpoint health --cluster >/dev/null 2>&1; }

# ============================================================
STEP=prep
mkdir -p "$WORK" "$BIN"
teardown # clear leftovers from any previous m7 run
rm -f "$WORK"/agent-m7-*.pid "$WORK"/*.log

for p in 9443 9445 4379 4381 4383; do
  if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    die "port $p is already in use; the m7 stack needs it free"
  fi
done
pass "m7 ports free (9443 9445 4379 4381 4383)"

# Build from a pinned pre-M4 commit by default. M4 agents (0ef2c42+) start
# infra pods with cluster-scoped STATIC addresses on the shared klite0
# network (10.44.0.<10+index>) plus host admin ports 19000+i/19500+i; a
# second cluster on the same Docker daemon collides with the canonical dev
# cluster in both directions. Until that's per-cluster, chaos runs stay on the
# pre-infra stack. Override with M7_REF=HEAD (dedicated daemon only) or
# M7_TREE=1 to build the working tree.
M7_REF="${M7_REF:-5e09b2c}"
TREE_BUILD=0
build_set() { (cd "$1" && go build -o "$BIN/klited" ./cmd/klited && go build -o "$BIN/klite" ./cmd/klite && go build -o "$BIN/klite-agent" ./cmd/klite-agent); }
if [ "${M7_TREE:-0}" = 1 ] && build_set "$REPO" >"$WORK/build.log" 2>&1; then
  TREE_BUILD=1
  pass "built klited, klite, klite-agent from the working tree (M7_TREE=1)"
else
  [ "${M7_TREE:-0}" = 1 ] && info "working tree build failed; using M7_REF instead"
  rm -rf "$WORK/src" && mkdir -p "$WORK/src"
  git -C "$REPO" archive "$M7_REF" | tar -x -C "$WORK/src" || die "git archive $M7_REF"
  build_set "$WORK/src" >"$WORK/build.log" 2>&1 || die "$M7_REF does not build (see $WORK/build.log)"
  pass "built klited, klite, klite-agent from $M7_REF"
fi

docker image inspect traefik/whoami:v1.10 >/dev/null 2>&1 || docker pull traefik/whoami:v1.10 >/dev/null 2>&1

# ============================================================
STEP=1-boot
docker network create "$ETCD_NET" >/dev/null 2>&1
ETCD_CLUSTER="etcd-m7-1=http://etcd-m7-1:2380,etcd-m7-2=http://etcd-m7-2:2380,etcd-m7-3=http://etcd-m7-3:2380"
# No volume mount, unlike etcd-up.sh: data lives in the container layer, so
# docker rm -f guarantees a fresh store every run (a reused data dir would
# silently resurrect the previous run's state) while stop/start in step 6
# still keeps member data.
for i in 1 2 3; do
  port=$((4379 + (i - 1) * 2))
  docker run -d --name "etcd-m7-$i" --network "$ETCD_NET" \
    --label io.klite.m7=1 \
    -p "127.0.0.1:$port:2379" \
    -e ETCD_UNSUPPORTED_ARCH=arm64 \
    "$ETCD_IMG" etcd \
    --name "etcd-m7-$i" \
    --data-dir /etcd-data \
    --initial-advertise-peer-urls "http://etcd-m7-$i:2380" \
    --listen-peer-urls http://0.0.0.0:2380 \
    --listen-client-urls http://0.0.0.0:2379 \
    --advertise-client-urls "http://etcd-m7-$i:2379" \
    --initial-cluster "$ETCD_CLUSTER" \
    --initial-cluster-state new \
    --initial-cluster-token klite-etcd-m7 \
    --auto-compaction-retention 1h >/dev/null || die "docker run etcd-m7-$i"
done
wait_for 30 etcd_healthy && pass "etcd trio healthy on $ETCD_EPS" || die "etcd trio healthy on $ETCD_EPS"

# klited A first so leadership lands on it deterministically, then B as standby.
"$BIN/klited" --listen "$EP_A" --etcd "$ETCD_EPS" >"$LOG_A" 2>&1 &
KLITED_A_PID=$!
disown
a_leads() { [ "$(leader_state "$LOG_A")" = leading ]; }
klited_a_serves() { "$KLITE" --server "$EP_A" get workloads >/dev/null 2>&1; }
wait_for 15 klited_a_serves && wait_for 10 a_leads \
  && pass "klited A ($EP_A) serving and leading" || die "klited A ($EP_A) serving and leading"
if [ "$TREE_BUILD" = 1 ]; then
  TOKEN=$("$KLITE" --server "$EP_A" node token) && pass "minted join token (M8 tree)" || die "mint join token"
fi
ROWS=0
for k in nodes workloads instances; do
  ROWS=$((ROWS + $("$KLITE" --server "$EP_A" get "$k" 2>/dev/null | tail -n +2 | grep -c .)))
done
[ "$ROWS" = 0 ] && pass "fresh etcd store is empty (no state leaked from earlier runs)" \
  || die "fresh etcd store already holds $ROWS object(s); teardown is leaking state"

"$BIN/klited" --listen "$EP_B" --etcd "$ETCD_EPS" >"$LOG_B" 2>&1 &
KLITED_B_PID=$!
disown
b_standby() { [ "$(leader_state "$LOG_B")" = standby ]; }
klited_b_serves() { "$KLITE" --server "$EP_B" get workloads >/dev/null 2>&1; }
wait_for 15 klited_b_serves && wait_for 10 b_standby \
  && pass "klited B ($EP_B) serving and standing by" || die "klited B ($EP_B) serving and standing by"
grep -q "controllers: leading" "$LOG_B" && die "both klited replicas claim leadership at boot"

"$KLITE" --server "$BOTH" apply -f - >/dev/null <<'EOF' || die "apply 3 node YAMLs"
apiVersion: klite/v1
kind: Node
metadata:
  name: m7-1
  labels:
    zone: local
spec:
  maxInstances: 32
---
apiVersion: klite/v1
kind: Node
metadata:
  name: m7-2
  labels:
    zone: local
spec:
  maxInstances: 32
---
apiVersion: klite/v1
kind: Node
metadata:
  name: m7-3
  labels:
    zone: local
spec:
  maxInstances: 32
EOF
pass "applied node YAMLs for m7-1 m7-2 m7-3"

# Known gap, re-probed each run: klite-agent takes exactly one --server address
# (grpc.NewClient rejects a comma list), so agents cannot fail over between
# klited replicas. All agents therefore pin to the standby (B), which this
# harness never kills; the leader kill in step 3 exercises controller failover.
"$BIN/klite-agent" --node m7-probe --server "$BOTH" >"$WORK/agent-probe.log" 2>&1 &
PROBE_PID=$!
disown
sleep 2
kill -9 "$PROBE_PID" 2>/dev/null
if grep -q "too many colons in address" "$WORK/agent-probe.log"; then
  info "confirmed: agent rejects multi-endpoint --server ('too many colons'); pinning agents to standby $EP_B"
else
  info "agent no longer rejects endpoint lists; consider pointing agents at $BOTH"
fi

for n in $NODES; do start_agent "$n"; done
wait_for 20 all_nodes_ready && pass "3 nodes Ready" || die "3 nodes Ready"

# ============================================================
STEP=2-baseline
# m7_web_yaml <WHOAMI_NAME value>: the workload spec. Tree builds add M5's
# fast drain knobs (4s, outside the template hash) so step 8's rollout fits
# its budget; the pinned pre-M5 ref predates the drain fields.
m7_web_yaml() {
  printf 'apiVersion: klite/v1\nkind: Workload\nmetadata:\n  name: m7-web\n  labels:\n    app: m7-web\nspec:\n'
  [ "$TREE_BUILD" = 1 ] && printf '  drain:\n    drainTimeoutSeconds: 4\n    terminationGraceSeconds: 4\n'
  printf '  replicas: 4\n  template:\n    labels:\n      app: m7-web\n    containers:\n      - name: web\n        image: traefik/whoami:v1.10\n        env:\n          - name: WHOAMI_NAME\n            value: %s\n        ports:\n          - containerPort: 80\n' "$1"
}
m7_web_yaml m7 | "$KLITE" --server "$BOTH" apply -f - >/dev/null || die "apply workload m7-web"
wait_for 90 converged_running 4 \
  && pass "m7-web at 4/4 Running, containers match instances" \
  || die "m7-web at 4/4 Running, containers match instances"
SPREAD=$(instances_pairs | awk -F/ '{print $1}' | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')
DISTINCT=$(instances_pairs | awk -F/ '{print $1}' | sort -u | grep -c .)
[ "$DISTINCT" -ge 2 ] && pass "instances spread across $DISTINCT nodes: $SPREAD" \
  || die "no spread: all instances on one node ($SPREAD)"

# ============================================================
STEP=3-leader-kill
# Bounce the workload between 6 and 8 replicas so the controllers are
# mid-create and mid-delete when the leader dies, then SIGKILL it. The churn
# keeps writing through the surviving replica across the takeover.
[ "$(leader_state "$LOG_A")" = leading ] || die "expected klited A to be leader before the kill"
( for _ in $(seq 1 45); do
    "$KLITE" --server "$BOTH" scale workload "$WL" --replicas 8 >/dev/null 2>&1; sleep 0.4
    "$KLITE" --server "$BOTH" scale workload "$WL" --replicas 6 >/dev/null 2>&1; sleep 0.4
  done ) &
CHURN_PID=$!
disown
scale_landed() {
  local r
  r=$("$KLITE" --server "$BOTH" get workloads 2>/dev/null | awk -v wl="$WL" '$1==wl {print $3}')
  [ "$r" = 8 ] || [ "$r" = 6 ]
}
wait_for 10 scale_landed || die "churn scale never reached the store"

B_OFF=$(wc -c <"$LOG_B" | tr -d ' ')
T_KILL=$(t_ms)
kill -9 "$KLITED_A_PID" || die "SIGKILL leader klited"
info "SIGKILLed leader klited A (pid $KLITED_A_PID) mid-scale-churn"

TAKEOVER_MS=""
while :; do
  if tail -c +"$((B_OFF + 1))" "$LOG_B" | grep -qF "controllers: leading"; then
    TAKEOVER_MS=$(( $(t_ms) - T_KILL ))
    break
  fi
  [ $(( $(t_ms) - T_KILL )) -ge 10000 ] && break
  sleep 0.1
done
[ -n "$TAKEOVER_MS" ] \
  && pass "standby took leadership ${TAKEOVER_MS}ms after SIGKILL (budget 10s: TTL 5s + margin)" \
  || die "standby did not log 'controllers: leading' within 10s of the kill"
kill -0 "$KLITED_A_PID" 2>/dev/null && die "old leader still alive after SIGKILL"

# Stop the churn and set the final target through the survivor: the new
# leader inherits a half-scaled store and must converge it to 8 itself.
kill "$CHURN_PID" 2>/dev/null
wait "$CHURN_PID" 2>/dev/null
CHURN_PID=""
sleep 1.5 # let any in-flight churn write land before the final target
"$KLITE" --server "$BOTH" scale workload "$WL" --replicas 8 >/dev/null 2>&1 \
  || die "post-kill scale to 8 through the survivor"

# Converge to 8 while proving agents never miss a beat: poll node readiness
# every second until the scale lands.
VIOL="$WORK/ready-violations.txt"
: >"$VIOL"
CONV_S=""
T0=$(date +%s)
while :; do
  rn=$(nodes_ready_count)
  [ "$rn" = 3 ] || echo "t=+$(( $(date +%s) - T0 ))s ready=$rn" >>"$VIOL"
  if converged 8; then CONV_S=$(( $(date +%s) - T0 )); break; fi
  [ $(( $(date +%s) - T0 )) -ge 60 ] && break
  sleep 1
done
[ -n "$CONV_S" ] \
  && pass "new leader converged the churned store to 8/8 Running in ${CONV_S}s, no dup or orphan containers" \
  || die "scale did not converge to 8 within 60s of the leader kill (see $VIOL and $WORK)"
[ -s "$VIOL" ] && die "nodes dipped below Ready during takeover: $(tr '\n' ' ' <"$VIOL")"
pass "all 3 nodes stayed Ready through the takeover window"

# ============================================================
STEP=4-cli-failover
"$KLITE" --server "$BOTH" get instances >/dev/null 2>&1 \
  && pass "get against the endpoint list succeeds with $EP_A down" \
  || die "get against the endpoint list succeeds with $EP_A down"
"$KLITE" --server "$BOTH" scale workload "$WL" --replicas 8 >/dev/null 2>&1 \
  && pass "scale against the endpoint list succeeds with $EP_A down" \
  || die "scale against the endpoint list succeeds with $EP_A down"
T0=$(date +%s)
if "$KLITE" --server "$EP_A" get nodes >/dev/null 2>&1; then
  die "get against the dead endpoint alone unexpectedly succeeded"
fi
DEAD_S=$(( $(date +%s) - T0 ))
[ "$DEAD_S" -le 20 ] \
  && pass "get against the dead endpoint alone fails within its deadline (${DEAD_S}s; WaitForReady holds until the 15s timeout)" \
  || die "get against the dead endpoint took ${DEAD_S}s (>20s) to fail"

# ============================================================
STEP=5-restart-klited
LOG_A="$WORK/klited-9443-restart.log"
"$BIN/klited" --listen "$EP_A" --etcd "$ETCD_EPS" >"$LOG_A" 2>&1 &
KLITED_A_PID=$!
disown
a_standby() { [ "$(leader_state "$LOG_A")" = standby ]; }
wait_for 15 klited_a_serves && wait_for 10 a_standby \
  && pass "restarted klited A serves and stands by" || die "restarted klited A serves and stands by"
grep -q "controllers: leading" "$LOG_A" && die "restarted klited grabbed leadership from a live leader"
"$KLITE" --server "$EP_A" get nodes >/dev/null 2>&1 \
  && pass "reads through the restarted replica alone" || die "reads through the restarted replica alone"
"$KLITE" --server "$EP_A" scale workload "$WL" --replicas 4 >/dev/null 2>&1 \
  && pass "writes through the restarted replica alone (scale 8 -> 4)" \
  || die "writes through the restarted replica alone (scale 8 -> 4)"
wait_for 45 converged_running 4 \
  && pass "converged back to 4/4 Running" || die "converged back to 4/4 Running"

# ============================================================
STEP=6-etcd-member-down
docker stop etcd-m7-2 >/dev/null || die "docker stop etcd-m7-2"
"$KLITE" --server "$BOTH" get nodes >/dev/null 2>&1 \
  && pass "get works with etcd-m7-2 down" || die "get works with etcd-m7-2 down"
"$KLITE" --server "$BOTH" scale workload "$WL" --replicas 4 >/dev/null 2>&1 \
  && pass "scale works with etcd-m7-2 down" || die "scale works with etcd-m7-2 down"
"$KLITE" --server "$BOTH" apply -f - >/dev/null 2>&1 <<'EOF' \
  && pass "apply works with etcd-m7-2 down" || die "apply works with etcd-m7-2 down"
apiVersion: klite/v1
kind: Node
metadata:
  name: m7-1
  labels:
    zone: local
spec:
  maxInstances: 32
EOF
OK=1
for _ in $(seq 1 15); do
  [ "$(nodes_ready_count)" = 3 ] || OK=0
  sleep 1
done
[ "$OK" = 1 ] && pass "agents unaffected: 3 nodes Ready across a 15s poll" \
  || die "agents unaffected: 3 nodes Ready across a 15s poll"
docker start etcd-m7-2 >/dev/null || die "docker start etcd-m7-2"
wait_for 30 etcd_healthy && pass "etcd back to full health" || die "etcd back to full health"

# Informational, not gating: drop TWO members so quorum is gone, expect the
# write to fail at its deadline, then restart immediately and require recovery.
info "quorum-loss probe (informational): stopping etcd-m7-2 and etcd-m7-3"
docker stop etcd-m7-2 etcd-m7-3 >/dev/null
"$KLITE" --server "$BOTH" get nodes >"$WORK/quorum-get.txt" 2>&1 &
QGET_PID=$!
T0=$(date +%s)
if "$KLITE" --server "$BOTH" scale workload "$WL" --replicas 4 >/dev/null 2>&1; then
  info "quorum-loss: write unexpectedly succeeded ($(( $(date +%s) - T0 ))s)"
else
  info "quorum-loss: write failed after $(( $(date +%s) - T0 ))s (deadline), as expected"
fi
if wait "$QGET_PID" 2>/dev/null; then
  info "quorum-loss: read still succeeded (served before the outage bit)"
else
  info "quorum-loss: read failed too (quorum reads degrade), as expected"
fi
docker start etcd-m7-2 etcd-m7-3 >/dev/null || die "docker start etcd-m7-2 etcd-m7-3"
T0=$(date +%s)
wait_for 30 etcd_healthy || die "etcd health after restarting both members"
recovered() { "$KLITE" --server "$BOTH" scale workload "$WL" --replicas 4 >/dev/null 2>&1 && all_nodes_ready; }
wait_for 45 recovered && pass "recovered after quorum loss: writes work, 3 nodes Ready" \
  || die "recovered after quorum loss: writes work, 3 nodes Ready"
# Quorum loss expires the leader's session, so both replicas end up standing
# by until the stale election key's lease is revoked; controllers must come
# back on their own.
leader_back() { [ "$(leader_state "$LOG_A")" = leading ] || [ "$(leader_state "$LOG_B")" = leading ]; }
wait_for 40 leader_back \
  && pass "controllers resumed: a leader re-established $(( $(date +%s) - T0 ))s after quorum restore" \
  || die "no klited re-took leadership within 40s of quorum restore"
wait_for 30 converged_running 4 || die "back to 4/4 Running after etcd chaos"

# ============================================================
STEP=7-agent-failover
# Kill the agent on the busiest node so reschedule and orphan cleanup both bite.
VICTIM=$(instances_pairs | awk -F/ '{print $1}' | sort | uniq -c | sort -rn | head -1 | awk '{print $2}')
[ -n "$VICTIM" ] || die "pick a victim node"
VPID=$(cat "$WORK/agent-$VICTIM.pid")
VCOUNT=$(instances_pairs | grep -c "^$VICTIM/")
info "victim: $VICTIM (pid $VPID) running $VCOUNT of 4 instances"
kill -9 "$VPID" || die "SIGKILL agent $VICTIM"
T0=$(date +%s)
notready() { node_not_ready "$VICTIM"; }
wait_for 20 notready || die "$VICTIM NotReady within 20s of agent SIGKILL"
NOTREADY_S=$(( $(date +%s) - T0 ))
pass "$VICTIM NotReady ${NOTREADY_S}s after agent SIGKILL (budget 20s)"

T1=$(date +%s)
rescheduled() {
  [ "$(running_count)" = 4 ] && [ "$(instances_pairs | grep -c "^$VICTIM/")" = 0 ]
}
wait_for 40 rescheduled || die "instances rescheduled off $VICTIM within 40s of NotReady"
RESCHED_S=$(( $(date +%s) - T1 ))
pass "all 4 instances Running on surviving nodes ${RESCHED_S}s after NotReady (budget 40s)"

ORPHANS=$(docker ps -aq --filter "label=io.klite.role=workload" --filter "label=io.klite.node=$VICTIM" | wc -l | tr -d ' ')
[ "$ORPHANS" -ge 1 ] && info "$ORPHANS orphan container(s) still on $VICTIM awaiting its agent" \
  || info "no leftover containers on $VICTIM (nothing to orphan-clean)"
start_agent "$VICTIM"
victim_ready() { "$KLITE" --server "$BOTH" get nodes 2>/dev/null | awk -v n="$VICTIM" '$1==n && $2=="Ready"' | grep -q .; }
wait_for 20 victim_ready && pass "$VICTIM Ready again after agent restart" \
  || die "$VICTIM Ready again after agent restart"
wait_for 30 converged_running 4 \
  && pass "orphans cleaned: containers match instance objects exactly" \
  || die "orphans cleaned: containers match instance objects exactly"

# ============================================================
STEP=8-rollout-resume
# Kill the leader mid-rollout and require the new leader to RESUME it: all 4
# instances land on exactly one new template hash, the count never leaves
# [replicas-1, replicas+surge] = [3, 5], and capacity never dips below 3
# serving (ADR 0010). Needs M5 rollouts, so the pinned pre-M5 ref skips.
if [ "$TREE_BUILD" = 1 ]; then
  wl_hashes() {
    local n
    for n in $NODES; do
      docker ps --filter "label=io.klite.node=$n" --filter "label=io.klite.workload=$WL" \
        --format '{{.Label "io.klite.template-hash"}}'
    done | sort -u
  }
  OLD_HASH="$(wl_hashes)"
  [ "$(printf '%s\n' "$OLD_HASH" | grep -c .)" = 1 ] || die "expected one template hash before the rollout"
  m7_web_yaml m7-v2 | "$KLITE" --server "$BOTH" apply -f - >/dev/null || die "apply m7-web v2 template"
  sleep 2 # old and new template hashes both live now

  if [ "$(leader_state "$LOG_A")" = leading ]; then
    LEAD_PID=$KLITED_A_PID; LEAD=A; SURV_LOG=$LOG_B
  else
    LEAD_PID=$KLITED_B_PID; LEAD=B; SURV_LOG=$LOG_A
  fi
  S_OFF=$(wc -c <"$SURV_LOG" | tr -d ' ')
  kill -9 "$LEAD_PID" || die "SIGKILL leader klited $LEAD mid-rollout"
  info "SIGKILLed leader klited $LEAD mid-rollout"
  [ "$LEAD" = A ] && KLITED_A_PID="" || KLITED_B_PID=""
  survivor_leads() { tail -c +"$((S_OFF + 1))" "$SURV_LOG" | grep -qF "controllers: leading"; }
  wait_for 10 survivor_leads \
    && pass "survivor took leadership mid-rollout (budget 10s)" \
    || die "survivor did not take leadership within 10s"

  VIOL=""
  DONE=""
  T0=$(date +%s)
  while :; do
    SNAP="$("$KLITE" --server "$BOTH" get instances 2>/dev/null | awk -v wl="$WL" '$2==wl {print $4}')"
    TOTAL=$(printf '%s\n' "$SNAP" | grep -c '[^ ]')
    SERVING=$(printf '%s\n' "$SNAP" | grep -Ec '^(Running|Ready|Draining)$')
    [ "$TOTAL" -le 5 ] || { VIOL="total $TOTAL > 5: surge exceeded 1"; break; }
    [ "$TOTAL" -ge 3 ] || { VIOL="total $TOTAL < 3: dipped below replicas-1"; break; }
    [ "$SERVING" -ge 3 ] || { VIOL="serving $SERVING < 3: capacity dipped"; break; }
    NEW_HASHES="$(wl_hashes)"
    if converged 4 && [ "$(printf '%s\n' "$NEW_HASHES" | grep -c .)" = 1 ] && [ "$NEW_HASHES" != "$OLD_HASH" ]; then
      DONE=1
      break
    fi
    [ $(( $(date +%s) - T0 )) -ge 120 ] && { VIOL="timeout: rollout did not finish under the new leader"; break; }
    sleep 0.5
  done
  if [ -z "$DONE" ]; then
    echo "--- rollout state at failure ---"
    "$KLITE" --server "$BOTH" get instances 2>/dev/null | awk -v wl="$WL" 'NR==1 || $2==wl'
    echo "hashes: $(wl_hashes | tr '\n' ' ') (old: $OLD_HASH)"
    die "rollout-resume invariant: $VIOL"
  fi
  pass "new leader resumed the rollout: 4/4 on one new hash in $(( $(date +%s) - T0 ))s, count stayed in [3,5]"
else
  echo "SKIP [$STEP]: rollout-resume-under-leader-kill — $M7_REF predates M5 rollouts (run with M7_TREE=1)"
fi

# ============================================================
STEP=9-teardown
teardown
LEFT=$(docker ps -aq --filter name=etcd-m7 | wc -l | tr -d ' ')
LEFT=$((LEFT + $(docker ps -aq --filter "name=klite.m7-" | wc -l | tr -d ' ')))
for n in $NODES; do
  LEFT=$((LEFT + $(docker ps -aq --filter "label=io.klite.node=$n" | wc -l | tr -d ' ')))
done
pgrep -f "$BIN/" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
docker network inspect "$ETCD_NET" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
[ "$LEFT" = 0 ] && pass "everything m7-scoped torn down (processes, containers, network)" \
  || die "teardown left $LEFT m7 artifact(s) behind"

echo
echo "verify-m7: all gating steps passed"
echo "timings: leader takeover ${TAKEOVER_MS}ms, converge-after-takeover ${CONV_S}s, NotReady ${NOTREADY_S}s, reschedule ${RESCHED_S}s"
