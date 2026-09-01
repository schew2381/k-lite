#!/usr/bin/env bash
# Checks M1 end to end: apply/get/scale/delete against real etcd, surviving a
# klited kill and an etcd member restart. Leaves etcd running when it's done.
set -u

cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

BIN=bin
KLITE="$BIN/klite"
KLITED1_PID=""
KLITED2_PID=""

pass() { echo "PASS: $1"; }
die()  { echo "FAIL: $1"; exit 1; }

cleanup() {
  [[ -n "$KLITED1_PID" ]] && kill "$KLITED1_PID" 2>/dev/null
  [[ -n "$KLITED2_PID" ]] && kill "$KLITED2_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT

wait_ready() {
  for _ in $(seq 1 30); do
    "$KLITE" --server "$1" get workloads >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

hack/etcd-up.sh >/dev/null 2>&1 \
  && pass "etcd cluster up" || die "etcd cluster up"

go build -o "$BIN/klited" ./cmd/klited && go build -o "$BIN/klite" ./cmd/klite \
  && pass "build klited and klite" || die "build klited and klite"

"$BIN/klited" --listen 127.0.0.1:7443 >/tmp/klited-7443.log 2>&1 &
KLITED1_PID=$!
wait_ready 127.0.0.1:7443 \
  && pass "klited answering on 7443" || die "klited answering on 7443"

# Data dirs persist across etcd-up runs, so clear leftovers from earlier attempts.
"$KLITE" delete -f examples/seed/apps/b-whoami.yaml >/dev/null 2>&1

"$KLITE" apply -f examples/seed/apps/b-whoami.yaml | grep -q "workload/b created" \
  && pass "apply b-whoami.yaml" || die "apply b-whoami.yaml"

"$KLITE" get workloads | grep -Eq '^b[[:space:]]+0/2[[:space:]]+2[[:space:]]' \
  && pass "get workloads shows b 0/2" || die "get workloads shows b 0/2"

"$KLITE" scale workload b --replicas 3 >/dev/null \
  && pass "scale workload b to 3" || die "scale workload b to 3"

"$KLITE" get workloads | grep -Eq '^b[[:space:]]+0/3[[:space:]]+3[[:space:]]' \
  && pass "get workloads shows replicas 3" || die "get workloads shows replicas 3"

"$BIN/klited" --listen 127.0.0.1:7445 >/tmp/klited-7445.log 2>&1 &
KLITED2_PID=$!
wait_ready 127.0.0.1:7445 \
  && pass "second klited answering on 7445" || die "second klited answering on 7445"

kill "$KLITED1_PID" && wait "$KLITED1_PID" 2>/dev/null
KLITED1_PID=""
pass "first klited killed"

"$KLITE" --server 127.0.0.1:7445 get workloads | grep -Eq '^b[[:space:]]' \
  && pass "get still answers via 7445" || die "get still answers via 7445"

docker restart etcd-2 >/dev/null \
  && pass "etcd member 2 restarted" || die "etcd member 2 restarted"

get_after_restart() {
  for _ in $(seq 1 20); do
    "$KLITE" --server 127.0.0.1:7445 get workloads | grep -Eq '^b[[:space:]]' && return 0
    sleep 0.5
  done
  return 1
}
get_after_restart \
  && pass "get answers through etcd member restart" || die "get answers through etcd member restart"

"$KLITE" --server 127.0.0.1:7445 delete -f examples/seed/apps/b-whoami.yaml | grep -q "workload/b deleted" \
  && pass "delete -f b-whoami.yaml" || die "delete -f b-whoami.yaml"

"$KLITE" --server 127.0.0.1:7445 get workloads | grep -Eq '^b[[:space:]]' \
  && die "workload b gone after delete" || pass "workload b gone after delete"

echo
echo "verify-m1: all steps passed (etcd left running)"
