#!/usr/bin/env bash
# Checks M3 end to end: klite logs (tail, follow, two concurrent follows,
# eof on container death) and klite describe, against workload a on two
# agents. Leaves etcd and the klite0 network running when it's done.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
KLITED_PID=""
AGENT1_PID=""
AGENT2_PID=""

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }

cleanup() {
  [[ -n "$AGENT1_PID" ]] && kill "$AGENT1_PID" 2>/dev/null
  [[ -n "$AGENT2_PID" ]] && kill "$AGENT2_PID" 2>/dev/null
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

start_agent() {
  "$BIN/klite-agent" --node "node-$1" >"/tmp/klite-agent-node-$1.log" 2>&1 &
}

klited_ready() { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready()  { [[ "$("$KLITE" get nodes | awk '$2=="Ready"' | wc -l | tr -d ' ')" == 2 ]]; }
a_running()    { "$KLITE" get instances | awk '$2=="a" && ($4=="Running" || $4=="Ready")' | grep -q a; }
tail3_ok() {
  "$KLITE" logs "$INST" --tail 3 >/tmp/klite-m3-tail.log 2>&1 || return 1
  [[ "$(wc -l </tmp/klite-m3-tail.log | tr -d ' ')" == 3 ]] && grep -q "tick" /tmp/klite-m3-tail.log
}

# --- fresh cluster state ---
hack/etcd-up.sh down >/dev/null 2>&1
rm -rf "$HOME/.klite/etcd"
hack/etcd-up.sh >/dev/null 2>&1 \
  && pass "fresh etcd cluster up" || die "fresh etcd cluster up"

go build -o "$BIN/klited" ./cmd/klited \
  && go build -o "$BIN/klite" ./cmd/klite \
  && go build -o "$BIN/klite-agent" ./cmd/klite-agent \
  && pass "build klited, klite, klite-agent" || die "build klited, klite, klite-agent"
docker image inspect busybox:1.36 >/dev/null 2>&1 || docker pull busybox:1.36 >/dev/null 2>&1

"$BIN/klited" --listen 127.0.0.1:7443 >/tmp/klited-7443.log 2>&1 &
KLITED_PID=$!
wait_for 15 klited_ready \
  && pass "klited answering on 7443" || die "klited answering on 7443"

# --- nodes ---
for i in 1 2; do
  "$KLITE" apply -f "examples/seed/nodes/node-$i.yaml" >/dev/null || die "apply node-$i.yaml"
done
pass "applied 2 node YAMLs"

start_agent 1; AGENT1_PID=$!
start_agent 2; AGENT2_PID=$!
wait_for 10 nodes_ready \
  && pass "both nodes Ready within 10s" || die "both nodes Ready within 10s"

# --- workload a: a ticker that prints one line a second ---
# The log-plumbing checks need a steady cadence. The seeded demo apps chat
# on a sparse per-wave roll, so this stays a dedicated inline fixture.
cat <<'EOF' | "$KLITE" apply -f - >/dev/null \
  && pass "apply the inline ticker workload a" || die "apply the inline ticker workload a"
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
      - name: tick
        image: busybox:1.36
        command: ["/bin/sh", "-c"]
        args:
          - 'while sleep 1; do echo "tick $(date +%T)"; done'
EOF
wait_for 30 a_running \
  && pass "instance of a Running" || die "instance of a Running"
INST="$("$KLITE" get instances | awk '$2=="a" {print $1}' | head -1)"
[[ -n "$INST" ]] && pass "resolved instance $INST" || die "resolve a's instance name"

# --- logs --tail ---
wait_for 30 tail3_ok \
  && pass "logs --tail 3 returns exactly 3 recent lines and exits" \
  || die "logs --tail 3 returns exactly 3 recent lines and exits"

# --- logs --follow: lines keep arriving, Ctrl-C exits cleanly ---
"$KLITE" logs "$INST" -f >/tmp/klite-m3-follow.log 2>&1 &
FOLLOW_PID=$!
sleep 1.5
EARLY="$(wc -l </tmp/klite-m3-follow.log | tr -d ' ')"
sleep 4
LATE="$(wc -l </tmp/klite-m3-follow.log | tr -d ' ')"
[[ "$LATE" -gt "$EARLY" ]] \
  && pass "follow streamed new lines for ~5s ($EARLY -> $LATE)" \
  || die "follow streamed new lines for ~5s ($EARLY -> $LATE)"
follow_gone() { ! kill -0 "$FOLLOW_PID" 2>/dev/null; }
kill -INT "$FOLLOW_PID"
wait_for 5 follow_gone || die "follow exits after Ctrl-C"
wait "$FOLLOW_PID" \
  && pass "follow exits 0 after Ctrl-C" || die "follow exits 0 after Ctrl-C"

# --- two concurrent follow streams ---
"$KLITE" logs "$INST" -f >/tmp/klite-m3-con1.log 2>&1 &
CON1_PID=$!
"$KLITE" logs "$INST" -f >/tmp/klite-m3-con2.log 2>&1 &
CON2_PID=$!
sleep 4
C1="$(wc -l </tmp/klite-m3-con1.log | tr -d ' ')"
C2="$(wc -l </tmp/klite-m3-con2.log | tr -d ' ')"
[[ "$C1" -ge 1 && "$C2" -ge 1 ]] \
  && pass "two concurrent follows both receive ($C1 and $C2 lines)" \
  || die "two concurrent follows both receive ($C1 and $C2 lines)"
cons_gone() { ! kill -0 "$CON1_PID" 2>/dev/null && ! kill -0 "$CON2_PID" 2>/dev/null; }
kill -INT "$CON1_PID" "$CON2_PID"
wait_for 5 cons_gone \
  && pass "concurrent follows exit after Ctrl-C" || die "concurrent follows exit after Ctrl-C"

# --- describe ---
"$KLITE" describe instance "$INST" >/tmp/klite-m3-desc-inst.log 2>&1 || die "describe instance runs"
grep -Eq '^Node: +node-[12]$' /tmp/klite-m3-desc-inst.log \
  && grep -Eq '^Phase: +(Running|Ready)$' /tmp/klite-m3-desc-inst.log \
  && grep -Eq '^IP: +10\.44\.' /tmp/klite-m3-desc-inst.log \
  && pass "describe instance shows node, phase, IP" \
  || die "describe instance shows node, phase, IP"

"$KLITE" describe workload a >/tmp/klite-m3-desc-wl.log 2>&1 || die "describe workload runs"
grep -q "$INST" /tmp/klite-m3-desc-wl.log \
  && pass "describe workload a lists $INST" || die "describe workload a lists $INST"

# --- container death mid-follow ends the stream with eof ---
"$KLITE" logs "$INST" -f >/tmp/klite-m3-kill-follow.log 2>&1 &
KFOLLOW_PID=$!
sleep 2
CTR="$(docker ps -q --filter "label=io.klite.instance=$INST" | head -1)"
[[ -n "$CTR" ]] || die "find a's container"
docker kill "$CTR" >/dev/null \
  && pass "docker kill a's container mid-follow" || die "docker kill a's container mid-follow"
kfollow_gone() { ! kill -0 "$KFOLLOW_PID" 2>/dev/null; }
wait_for 10 kfollow_gone \
  || { kill "$KFOLLOW_PID" 2>/dev/null; die "follow ends with eof after container death (still hanging)"; }
wait "$KFOLLOW_PID" \
  && pass "follow ended cleanly with eof after container death" \
  || die "follow ended cleanly with eof after container death"

echo
echo "verify-m3: all steps passed (etcd and klite0 left running)"
