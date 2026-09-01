#!/usr/bin/env bash
# The k-lite demo runs end to end on the canonical stack (klited :7443/:7445,
# etcd :2379/81/83, nodes node-1..3). It gates PASS/FAIL like the verify
# scripts and paces itself for watching.
#
# Beats, in order:
#   wipe        fresh store, fresh CA, fresh identities
#   boot        etcd trio plus two stateless klited replicas
#   join        three nodes trade a token for an mTLS identity
#   apps        a, b, c go Ready: each serves its hostname and chats
#   discovery   a finds b by name through kdns and the VIP
#   scale       c grows 2 -> 3 live, landing the resting shape
#   rollout     every b replaced, probe loop provably clean
#   policy      deny a -> c on both planes (chatter included), restore
#   drain       a node empties surge-first, stream on screen
#   leader kill SIGKILL mid-scale, the survivor converges it
#   security    TLS 1.3 chain, ingress allocations, plaintext dies
#   finale      the apps' own chatter confirmed, then the live board
#
# Zero-failure gates ride deterministic probe loops (wget to b and to c once
# a second from inside a's container). The apps' 5% random chatter is shown
# and checked on its own clock, never used as a gate's denominator.
#
# The demo leaves everything running so the audience can poke at it. Tear
# down with the cheat sheet it prints at the end (or hack/dev-down.sh --all).
# KLITE_DEMO_BEAT overrides the pause length between beats (default 2s).
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
EP_A=127.0.0.1:7443
EP_B=127.0.0.1:7445
export KLITE_SERVER="$EP_A,$EP_B"
NODES=(node-1 node-2 node-3)
SRV_DIR="$HOME/.klite/server"
AGT_DIR="$HOME/.klite/agent"
DEV_DIR="$HOME/.klite/dev"
LOG_A="$DEV_DIR/klited-7443.log"
LOG_B="$DEV_DIR/klited-7445.log"
INGRESS_BASE=20000
INGRESS_PER_NODE=32
BEAT="${KLITE_DEMO_BEAT:-2}"
STEP=preflight

pass() { echo "PASS [$STEP]: $1"; }
info() { echo "INFO [$STEP]: $1"; }
die()  {
  stop_probes 2>/dev/null
  echo "FAIL [$STEP]: $1"
  echo "The stack is left as-is for debugging (logs in $DEV_DIR)."
  echo "Reset with: hack/dev-down.sh --all"
  exit 1
}

banner() {
  echo
  echo "============================================================"
  echo "  $1"
  echo "============================================================"
}

# show <cmd...>: run a command the way a presenter would type it.
show() {
  echo
  echo "  \$ $*"
  "$@" 2>&1 | sed 's/^/  /'
  echo
}

pause() { sleep "$BEAT"; }

wait_for() { # wait_for <seconds> <fn> [args...]
  local budget=$1; shift
  for _ in $(seq 1 $((budget * 2))); do
    "$@" && return 0
    sleep 0.5
  done
  return 1
}

# --- cluster read helpers, all through the CLI --------------------------------
klited_a_ready() { "$KLITE" --server "$EP_A" get workloads >/dev/null 2>&1; }
klited_b_ready() { "$KLITE" --server "$EP_B" get workloads >/dev/null 2>&1; }
nodes_ready()   { [[ "$("$KLITE" get nodes 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 3 ]]; }
infra_up() {
  local n
  for n in "${NODES[@]}"; do
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.net" || return 1
    docker ps --format '{{.Names}}' | grep -qx "klite.$n.envoy" || return 1
  done
}
counts_ready() { # counts_ready <b> <c>: a=1 plus these counts, all Ready
  local snap; snap="$("$KLITE" get instances 2>/dev/null)"
  [[ "$(echo "$snap" | awk '$2=="a" && $4=="Ready"' | wc -l | tr -d ' ')" == 1 ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="b" && $4=="Ready"' | wc -l | tr -d ' ')" == "$1" ]] || return 1
  [[ "$(echo "$snap" | awk '$2=="c" && $4=="Ready"' | wc -l | tr -d ' ')" == "$2" ]]
}
# The resting shape every beat returns to, and what the board shows at the end.
steady() { counts_ready 2 3; }
a_ctr() { echo "klite.$A_NODE.$A_INST"; }
alloc_row() { "$KLITE" get ingressallocations 2>/dev/null | awk -v s="$1" -v i="$2" '$2==s && $3==i {print $4, $5}'; }

# --- deterministic probes ------------------------------------------------------
# Each loop fires one wget a second at its service from inside a's container,
# taking the same kdns -> VIP -> Envoy path as the apps' own chatter. Every
# attempt appends one line: the served body ("<cid> is b") or FAILED.
# Zero-failure gates diff the FAILED counts over a window and demand growth.
PROBE_B="$DEV_DIR/probe-b.log"
PROBE_C="$DEV_DIR/probe-c.log"
PROBE_PIDS=()
probe_loop() { # probe_loop <file> <url>
  local file=$1 url=$2
  while :; do
    docker exec "$(a_ctr)" wget -qO- -T 2 "$url" 2>/dev/null || echo "FAILED"
    sleep 1
  done >>"$file" 2>/dev/null
}
start_probes() {
  : >"$PROBE_B"
  : >"$PROBE_C"
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
leader_state() { # leader_state <log>: leading | standby | none
  local line
  line=$(grep -E "controllers: (leading|standing by|leadership released)" "$1" 2>/dev/null | tail -1)
  case "$line" in
    *"controllers: leading"*) echo leading ;;
    *"controllers: standing by"*) echo standby ;;
    *) echo none ;;
  esac
}
kill_port() { # kill_port <port>: free a TCP port we own (stale demo leftovers)
  local pids
  pids="$(lsof -ti tcp:"$1" 2>/dev/null)"
  [[ -n "$pids" ]] && kill $pids 2>/dev/null && sleep 1
  return 0
}

# ============================================================
banner "0. PREFLIGHT"
STEP=preflight

colima status >/dev/null 2>&1 || die "colima isn't running (run: make bootstrap)"
docker info >/dev/null 2>&1 || die "docker daemon unreachable (run: make bootstrap)"
for img in quay.io/coreos/etcd:v3.5.16 envoyproxy/envoy:v1.31.5; do
  docker image inspect "$img" >/dev/null 2>&1 || die "image $img missing (run: make bootstrap)"
done
# The apps' image is small enough to fetch on the spot when bootstrap predates it.
docker image inspect busybox:1.36 >/dev/null 2>&1 || docker pull busybox:1.36 >/dev/null 2>&1 \
  || die "image busybox:1.36 missing and the pull failed (run: make bootstrap)"
pass "colima up, docker answering, base images present"

command -v bun >/dev/null 2>&1 || die "bun missing for the web board (run: make bootstrap)"
if [[ ! -d frontend/node_modules ]]; then
  info "installing frontend dependencies (first run)"
  (cd frontend && bun install --frozen-lockfile) >/dev/null 2>&1 || die "bun install in frontend/"
fi
pass "bun present, frontend dependencies installed"

# ============================================================
banner "1. WIPE STALE STATE (fresh store, fresh CA, fresh identities)"
STEP=wipe

# A CA left over from an earlier cluster would poison this one's trust:
# agents holding old certificates dial in, the new klited rejects them, and
# every join beat below turns into a debugging session. So the server dir,
# the agent identities, and the etcd data all go.
hack/dev-down.sh --all >/dev/null 2>&1 || true
for f in "$DEV_DIR/facade.pid" "$DEV_DIR/vite.pid"; do
  [[ -f "$f" ]] && kill "$(cat "$f")" 2>/dev/null
  rm -f "$f"
done
pkill -f 'bin/klited' 2>/dev/null
pkill -f 'bin/klite-agent' 2>/dev/null
sleep 0.5
for n in "${NODES[@]}"; do
  docker ps -aq --filter "label=io.klite.node=$n" | xargs docker rm -f >/dev/null 2>&1
done
rm -rf "$SRV_DIR" "$AGT_DIR" "$DEV_DIR"
rm -rf "$HOME/.klite/etcd/etcd-1" "$HOME/.klite/etcd/etcd-2" "$HOME/.klite/etcd/etcd-3"
mkdir -p "$DEV_DIR"
pass "previous playgrounds torn down, ~/.klite/{server,agent,dev} and etcd data gone"

kill_port 7080 # a stale facade would shadow the one we launch at the end
kill_port 5173 # a stale Vite would grab the port and serve an old bundle
for p in 7443 7445 7080 5173; do
  lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && die "port $p is still in use"
done
for p in $(seq $INGRESS_BASE $((INGRESS_BASE + 3 * INGRESS_PER_NODE - 1))); do
  lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && die "ingress port $p is already in use"
done
pass "canonical ports free (7443, 7445, 7080, 5173, ingress $INGRESS_BASE-$((INGRESS_BASE + 3 * INGRESS_PER_NODE - 1)))"

STEP=build
make build >/dev/null 2>&1 && pass "built klited, klite, klite-agent" || die "make build"
if ! docker image inspect klite-net:dev >/dev/null 2>&1; then
  make net-image >/dev/null 2>&1 && pass "built the klite-net:dev image" || die "make net-image"
fi

# ============================================================
banner "2. CONTROL PLANE: etcd trio + TWO stateless klited replicas"
STEP=control-plane

hack/etcd-up.sh >/dev/null 2>&1 && pass "3-member etcd healthy on 127.0.0.1:2379/2381/2383" || die "etcd trio up"

"$BIN/klited" --listen "$EP_A" >"$LOG_A" 2>&1 &
echo $! >"$DEV_DIR/klited-7443.pid"
disown
wait_for 15 klited_a_ready || die "klited A not serving on $EP_A (see $LOG_A)"
a_leads() { [[ "$(leader_state "$LOG_A")" == leading ]]; }
wait_for 10 a_leads && pass "klited A serving on $EP_A and leading the controllers" || die "klited A leading"

"$BIN/klited" --listen "$EP_B" >"$LOG_B" 2>&1 &
echo $! >"$DEV_DIR/klited-7445.pid"
disown
wait_for 15 klited_b_ready || die "klited B not serving on $EP_B (see $LOG_B)"
b_standby() { [[ "$(leader_state "$LOG_B")" == standby ]]; }
wait_for 10 b_standby && pass "klited B serving on $EP_B, standing by (same CA, same admin token)" || die "klited B standby"

info "first boot minted the cluster's trust anchor:"
show ls -l "$SRV_DIR/tls" "$SRV_DIR/token"
pause

# ============================================================
banner "3. THREE NODES JOIN: a token in, an mTLS identity out"
STEP=join

TOKEN="$("$KLITE" node token)" || die "mint join token"
echo "  \$ klite node token"
echo "  $TOKEN"
echo
info "the K10<ca-sha256> prefix pins the CA, and ::node:<secret> is the one-time join proof"
pause

for i in 1 2 3; do
  "$KLITE" apply -f "examples/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "declared node-1..3 (membership is YAML first, ADR 0018)"

for n in "${NODES[@]}"; do
  "$BIN/klite-agent" --node "$n" --server "$KLITE_SERVER" --token "$TOKEN" >"$DEV_DIR/agent-$n.log" 2>&1 &
  echo $! >"$DEV_DIR/agent-$n.pid"
  disown
done
wait_for 30 nodes_ready && pass "3 agents joined, all nodes Ready" || die "3 nodes Ready (logs in $DEV_DIR)"
show "$KLITE" get nodes

info "what the join left on node-1's disk:"
show ls -l "$AGT_DIR/node-1/tls"
for n in "${NODES[@]}"; do
  d="$AGT_DIR/$n/tls"
  for f in node.key node.crt ca.crt; do
    [[ "$(stat -f '%Lp' "$d/$f" 2>/dev/null)" == "600" ]] || die "$d/$f missing or not 0600"
  done
  openssl verify -CAfile "$SRV_DIR/tls/ca.crt" "$d/node.crt" >/dev/null 2>&1 \
    || die "$n's certificate does not chain to the cluster CA"
done
show openssl x509 -in "$AGT_DIR/node-1/tls/node.crt" -noout -subject -ext extendedKeyUsage
pass "per-node identities: 0600 trio, CA-chained, CN=klite:node:<name>, client+server EKUs (ADR 0036)"
wait_for 90 infra_up && pass "infra pods running on every node (klite-net + Envoy, ADR 0008)" || die "infra pods up"
pause

# ============================================================
banner "4. DECLARE THE APPS: a, b, c each serve their name and chat"
STEP=apps

# Demo pace lives in the YAML, never in code (ADR 0010): 4s drain knobs go
# in outside the template so the hash and the choreography stay untouched.
mkdir -p "$DEV_DIR/apps"
patch_drain() { # patch_drain <src> <dst>
  awk 'BEGIN{done=0} {print} /^spec:$/ && !done {
    print "  drain:"; print "    drainTimeoutSeconds: 4"; print "    terminationGraceSeconds: 4"; done=1
  }' "$1" > "$2"
}
for app in a-client b-whoami c-whoami; do
  patch_drain "examples/apps/$app.yaml" "$DEV_DIR/apps/$app.yaml"
  "$KLITE" apply -f "$DEV_DIR/apps/$app.yaml" >/dev/null || die "apply $app.yaml"
done
wait_for 120 counts_ready 2 2 && pass "a, b, c all Ready (readiness probes gate all three)" || die "workloads Ready"
show "$KLITE" get instances
A_INST="$("$KLITE" get instances | awk '$2=="a" {print $1}' | head -1)"
A_NODE="$("$KLITE" get instances | awk '$2=="a" {print $3}' | head -1)"
[[ -n "$A_INST" && -n "$A_NODE" ]] || die "resolve a's instance and node"
info "every instance serves its hostname on :80 and rolls a 5% die each second"
info "a hit calls one random peer from TARGETS, the chatter the board feeds on"
start_probes
pause

# ============================================================
banner "5. DISCOVERY: a finds b by name, through its node's DNS and VIP"
STEP=discovery

# The gate reads the same lines the audience is about to see: the last four
# probes, all answered, reaching at least two distinct b endpoints. The
# earliest probes can show FAILED (the loop starts before the mesh finishes
# programming), and those must have scrolled away before this beat is worth
# watching.
b_lb() {
  local lines
  lines="$(tail -n 4 "$PROBE_B" 2>/dev/null)"
  [[ "$(grep -c ' is b' <<<"$lines")" == 4 ]] || return 1
  [[ "$(awk '{print $1}' <<<"$lines" | sort -u | wc -l | tr -d ' ')" -ge 2 ]]
}
wait_for 60 b_lb || { tail -n 5 "$PROBE_B"; die "a's probes reach both b endpoints"; }
info "the last four answers to wget http://b:8080, run once a second inside a's container:"
show tail -n 4 "$PROBE_B"
pass "requests balance across b's endpoints (the hostnames are container ids)"

info "the name b resolves inside a's container to this node's VIP:"
show docker exec "$(a_ctr)" nslookup b.svc.klite
docker exec "$(a_ctr)" nslookup b.svc.klite 2>&1 | grep -Eq 'Address: *10\.44\.(6[4-9]|[7-9][0-9]|1[0-1][0-9]|12[0-7])\.' \
  && pass "b.svc.klite answers with a VIP from 10.44.64.0/18 (ADR 0006)" \
  || die "nslookup did not return a pool VIP"
info "every (service, node) pair owns a VIP, stored like any other object:"
show "$KLITE" get vipallocations
pause

# ============================================================
banner "6. SCALE c LIVE: 2 -> 3"
STEP=scale

# This lands the cluster on its resting shape (a=1, b=2, c=3), which every
# later beat returns to and the board shows at the end.
ROLL_B_F0="$(fails_in "$PROBE_B")"; ROLL_C_F0="$(fails_in "$PROBE_C")"
ROLL_B_L0="$(lines_in "$PROBE_B")"; ROLL_C_L0="$(lines_in "$PROBE_C")"
show "$KLITE" scale workload c --replicas 3
wait_for 90 steady && pass "c converged to 3/3 Ready" || die "scale c to 3"
show "$KLITE" get instances
[[ "$("$KLITE" get instances | awk '$2=="c" {print $3}' | sort -u | wc -l | tr -d ' ')" -ge 2 ]] \
  && pass "c spans multiple nodes (spread-by-count scheduler, ADR 0012)" \
  || die "c landed on a single node"
pause

# ============================================================
banner "7. ROLLING UPDATE: replace all of b, drop zero requests"
STEP=rollout

# A template change (new env var) triggers the surge-first replace. The
# re-applied file still says replicas: 2, matching the stored count, so the
# rollout is the only change in flight.
B_BEFORE="$("$KLITE" get instances | awk '$2=="b" {print $1}' | sort)"
awk '{print} /^            value: a c$/ {print "          - name: DEMO_ROLLOUT"; print "            value: \"1\""}' \
  "$DEV_DIR/apps/b-whoami.yaml" | "$KLITE" apply -f - >/dev/null || die "apply the rolled b template"
info "watching the surge-first replace (create one, wait Ready, drain one, repeat)"
rolled() {
  steady || return 1
  local left
  left="$(comm -12 <(echo "$B_BEFORE") <("$KLITE" get instances 2>/dev/null | awk '$2=="b" {print $1}' | sort) | grep -c . || true)"
  [[ "$left" == 0 ]]
}
wait_for 180 rolled && pass "both b instances replaced with the new template, all Ready" || die "rollout converged"
sleep 3
FAILS=$(( $(fails_in "$PROBE_B") - ROLL_B_F0 + $(fails_in "$PROBE_C") - ROLL_C_F0 ))
[[ "$FAILS" == 0 && "$(lines_in "$PROBE_B")" -gt "$ROLL_B_L0" && "$(lines_in "$PROBE_C")" -gt "$ROLL_C_L0" ]] \
  && pass "ZERO failed requests across the scale and the rollout (the probe loops are the witness)" \
  || { tail -n 3 "$PROBE_B" "$PROBE_C"; die "$FAILS failed request(s) during the rollout"; }
pause

# ============================================================
banner "8. POLICY: deny a -> c live, both planes agree, then restore"
STEP=policy

info "right now a reaches both b and c (the last probe against each):"
show tail -n 1 "$PROBE_B" "$PROBE_C"
CHAT_MARK="$(date +%s)"
show "$KLITE" apply -f examples/policies/deny-a-to-c.yaml
c_denied() {
  [[ "$(tail -n 3 "$PROBE_C" 2>/dev/null | grep -c FAILED)" == 3 ]] \
    && tail -n 1 "$PROBE_B" 2>/dev/null | grep -q ' is b'
}
wait_for 30 c_denied && pass "the data plane turned: c probes FAILED while b keeps answering" || die "deny-a-to-c enforced"
show tail -n 3 "$PROBE_B" "$PROBE_C"
OUT="$("$KLITE" policy check a c 2>&1)"
grep -q 'denied' <<<"$OUT" && grep -q 'deny-a-to-c' <<<"$OUT" \
  && pass "control plane agrees and names the policy: $OUT" \
  || die "policy check a c should deny by deny-a-to-c, got: $OUT"
OUT="$("$KLITE" policy check a b 2>&1)"
grep -q 'allowed' <<<"$OUT" && pass "and a -> b stays open: $OUT" || die "policy check a b should allow, got: $OUT"

# The deny must bite the apps' own chatter as well as the probes. Chatter is
# sparse (a 5% roll), so denials appear only when a roll picks c, but
# completions must sit at zero from the moment of the apply.
CHAT_OK="$(docker logs "$(a_ctr)" --since "$CHAT_MARK" 2>/dev/null | grep -c '^-> c ok')"
CHAT_FAIL="$(docker logs "$(a_ctr)" --since "$CHAT_MARK" 2>/dev/null | grep -c '^-> c FAILED')"
[[ "$CHAT_OK" == 0 ]] \
  && pass "a's chatter to c since the deny: $CHAT_FAIL denied, 0 completed" \
  || die "$CHAT_OK chatty a -> c call(s) slipped past the policy"
pause

show "$KLITE" delete networkpolicy deny-a-to-c
c_back() { [[ "$(tail -n 2 "$PROBE_C" 2>/dev/null | grep -c ' is c')" == 2 ]]; }
wait_for 30 c_back && pass "policy deleted, a -> c flows again" || die "traffic restored after the delete"
pause

# ============================================================
banner "9. DRAIN A NODE: surge first, stream the progress, drop nothing"
STEP=drain

# One failure window spans this beat and the next: the drain, the leader
# kill, and both scales all answer to the same baseline.
CHAOS_B_F0="$(fails_in "$PROBE_B")"; CHAOS_C_F0="$(fails_in "$PROBE_C")"
CHAOS_B_L0="$(lines_in "$PROBE_B")"; CHAOS_C_L0="$(lines_in "$PROBE_C")"
chaos_fails() { echo $(( $(fails_in "$PROBE_B") - CHAOS_B_F0 + $(fails_in "$PROBE_C") - CHAOS_C_F0 )); }
chaos_grew()  { [[ "$(lines_in "$PROBE_B")" -gt "$CHAOS_B_L0" && "$(lines_in "$PROBE_C")" -gt "$CHAOS_C_L0" ]]; }
DRAIN_NODE=""
for cand in node-2 node-3 node-1; do
  [[ "$cand" == "$A_NODE" ]] && continue
  "$KLITE" get instances | awk -v n="$cand" '($2=="b" || $2=="c") && $3==n' | grep -q . && { DRAIN_NODE="$cand"; break; }
done
[[ -n "$DRAIN_NODE" ]] || die "no node hosting b or c apart from a's own"
info "draining $DRAIN_NODE (it hosts endpoints a is dialing right now)"
echo
echo "  \$ klite drain $DRAIN_NODE"
"$KLITE" drain "$DRAIN_NODE" 2>&1 | tee "$DEV_DIR/drain.log" | sed 's/^/  /'
echo
grep -q "^cordoned $DRAIN_NODE" "$DEV_DIR/drain.log" || die "drain stream shows the cordon"
grep -Eq '^draining ' "$DEV_DIR/drain.log" || die "drain stream shows per-instance draining"
grep -q "^done: $DRAIN_NODE drained" "$DEV_DIR/drain.log" || die "drain stream ends with done"
pass "nomad-style stream: cordoned, surged, draining, done"
show "$KLITE" get nodes
show "$KLITE" uncordon "$DRAIN_NODE"
wait_for 90 steady && pass "cluster re-converged with $DRAIN_NODE schedulable again" || die "re-converge after uncordon"
sleep 3
[[ "$(chaos_fails)" == 0 ]] && chaos_grew \
  && pass "ZERO failed requests through the whole drain" \
  || die "failed or stalled probes during the drain ($(chaos_fails) FAILED)"
pause

# ============================================================
banner "10. KILL THE LEADING klited MID-SCALE: the survivor converges it"
STEP=leader-kill

if [[ "$(leader_state "$LOG_A")" == leading ]]; then
  LEAD_PIDFILE="$DEV_DIR/klited-7443.pid"; LEAD_EP=$EP_A; LEAD_PORT=7443
  SURV_LOG=$LOG_B; SURV_EP=$EP_B
else
  LEAD_PIDFILE="$DEV_DIR/klited-7445.pid"; LEAD_EP=$EP_B; LEAD_PORT=7445
  SURV_LOG=$LOG_A; SURV_EP=$EP_A
fi
LEAD_PID="$(cat "$LEAD_PIDFILE")"
S_OFF=$(wc -c <"$SURV_LOG" | tr -d ' ')
"$KLITE" scale workload b --replicas 4 >/dev/null || die "scale b to 4"
kill -9 "$LEAD_PID" || die "SIGKILL the leader"
info "scaled b to 4 and SIGKILLed the leader ($LEAD_EP, pid $LEAD_PID) in the same breath"
survivor_leads() { tail -c +"$((S_OFF + 1))" "$SURV_LOG" | grep -qF "controllers: leading"; }
T0=$(date +%s)
wait_for 15 survivor_leads \
  && pass "the survivor took leadership $(( $(date +%s) - T0 ))s after the kill (etcd lease election, ADR 0005)" \
  || die "survivor never logged 'controllers: leading'"
b4c3() { counts_ready 4 3; }
wait_for 90 b4c3 && pass "scale to 4 converged through the survivor alone" || die "post-kill convergence"
show "$KLITE" --server "$SURV_EP" get instances
show "$KLITE" scale workload b --replicas 2
wait_for 90 steady && pass "and back down: the cluster rests at a=1, b=2, c=3" || die "scale b back to 2"
sleep 3
[[ "$(chaos_fails)" == 0 ]] && chaos_grew \
  && pass "ZERO failed requests across drain + leader kill + both scales (the data plane never blinked)" \
  || die "failed or stalled probes across the chaos window ($(chaos_fails) FAILED)"

"$BIN/klited" --listen "127.0.0.1:$LEAD_PORT" >"$DEV_DIR/klited-$LEAD_PORT.log" 2>&1 &
echo $! >"$LEAD_PIDFILE"
disown
[[ "$LEAD_PORT" == 7443 ]] && LOG_A="$DEV_DIR/klited-7443.log" || LOG_B="$DEV_DIR/klited-7445.log"
restarted_ready() { "$KLITE" --server "$LEAD_EP" get workloads >/dev/null 2>&1; }
wait_for 15 restarted_ready && pass "the killed replica is back on $LEAD_EP, standing by" || die "restart the killed klited"
pause

# ============================================================
banner "11. THE WIRE: TLS 1.3 everywhere, plaintext dies at the door"
STEP=security

SCLIENT="$(echo | openssl s_client -connect "$EP_A" -CAfile "$SRV_DIR/tls/ca.crt" -showcerts 2>/dev/null)"
grep -q "Verify return code: 0 (ok)" <<<"$SCLIENT" || die "klited's serving cert does not verify against the cluster CA"
grep -q "TLSv1.3" <<<"$SCLIENT" || die "klited did not negotiate TLS 1.3"
[[ "$(grep -c 'BEGIN CERTIFICATE' <<<"$SCLIENT")" == 2 ]] || die "server chain should be [leaf, CA]"
echo "$SCLIENT" | grep -E 'TLSv1.3|Verify return code' | sort -u | sed 's/^ */  /'
pass "klited speaks TLS 1.3 and presents the [leaf, CA] chain token pinning depends on"
pause

info "cross-node traffic rides per-endpoint mTLS ingress ports (ADRs 0034, 0035):"
# The beat before this one scaled b down, and the drained instances hold
# their ports until deletion, so give the allocator a beat to settle.
allocs_settled() { [[ "$("$KLITE" get ingressallocations 2>/dev/null | tail -n +2 | grep -c .)" == 6 ]]; }
wait_for 60 allocs_settled || { "$KLITE" get ingressallocations; die "want 6 ingress allocations (a=1, b=2, c=3)"; }
show "$KLITE" get ingressallocations
pass "6 allocations while traffic flows (a=1, b=2, c=3), each inside its node's slice"

B_PORT="$("$KLITE" get ingressallocations | awk '$2=="b" {print $5; exit}')"
[[ -n "$B_PORT" ]] || die "no ingress port to probe"
if curl -s --max-time 3 "http://127.0.0.1:${B_PORT}/" >/dev/null 2>&1; then
  die "a plaintext dial to ingress port $B_PORT got an answer"
fi
pass "plaintext dial to ingress port $B_PORT dies in the handshake (client certs or nothing)"
pause

# ============================================================
banner "12. FINALE: the apps' own chatter, then the live board"
STEP=finale

# The probes made the gates provable. The chatter keeps the board alive
# after this script ends. Minutes into the run, every app must have rolled
# its way into at least one completed call.
chatty_flowing() {
  local wl
  for wl in a b c; do
    docker ps --filter "label=io.klite.workload=$wl" --format '{{.Names}}' \
      | xargs -I{} docker logs {} 2>/dev/null | grep -q '^-> [abc] ok$' || return 1
  done
}
wait_for 90 chatty_flowing \
  && pass "every app chats on its own: a, b, and c each completed a random call" \
  || die "an app has not completed a single chatty call"
info "a's chatter so far:"
echo
echo "  \$ docker logs $(a_ctr) | grep '^->' | tail -4"
docker logs "$(a_ctr)" 2>/dev/null | grep '^->' | tail -4 | sed 's/^/  /'
echo
stop_probes

go run ./cmd/klite-facade >"$DEV_DIR/facade.log" 2>&1 &
echo $! >"$DEV_DIR/facade.pid"
disown
facade_up() { curl -s --max-time 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:7080/api/topology" | grep -q 200; }
wait_for 60 facade_up \
  && pass "klite-facade on :7080, dialing klited over CA-pinned TLS with the admin token" \
  || { tail -5 "$DEV_DIR/facade.log"; die "facade never answered on :7080 (see $DEV_DIR/facade.log)"; }

kill_port 5173 # nothing may squat the port, or Vite silently takes 5174
# NO_COLOR keeps the startup banner grep-able. The probe uses localhost
# because Vite may bind the IPv6 loopback only.
(cd frontend && NO_COLOR=1 exec bun run dev) >"$DEV_DIR/vite.log" 2>&1 &
echo $! >"$DEV_DIR/vite.pid"
disown
vite_up() { curl -s --max-time 2 -o /dev/null "http://localhost:5173/"; }
wait_for 60 vite_up || { tail -5 "$DEV_DIR/vite.log"; die "Vite never answered on :5173 (see $DEV_DIR/vite.log)"; }
grep -q "localhost:5173/" "$DEV_DIR/vite.log" || die "Vite came up on the wrong port (see $DEV_DIR/vite.log)"
pass "Vite dev server on :5173, /api proxied to the facade"

open "http://localhost:5173/#/?mode=live"
pass "opened the board in live mode"

# ============================================================
banner "THE CLUSTER IS YOURS"
echo "
  board       http://localhost:5173/#/?mode=live   (the MOCK|LIVE toggle in the header swaps the data source)
  processes   2x klited (7443/7445), 3 agents, facade (7080), vite (5173) — pidfiles in $DEV_DIR
  logs        $DEV_DIR/*.log

  poke at it:
    export KLITE_SERVER=$KLITE_SERVER
    $KLITE get instances --watch
    $KLITE logs -f $A_INST                              # a's chatter: '-> b ok'
    $KLITE scale workload b --replicas 3
    $KLITE apply -f examples/policies/deny-a-to-c.yaml   # watch the board turn
    $KLITE delete networkpolicy deny-a-to-c
    $KLITE drain $DRAIN_NODE && $KLITE uncordon $DRAIN_NODE
    $KLITE get ingressallocations
    $KLITE describe workload b

  tear down (everything, frontend included):
    make down
"
echo "demo: all beats passed"
