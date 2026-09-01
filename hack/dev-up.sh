#!/usr/bin/env bash
# One command to a running k-lite playground: etcd + klited + agents + example apps.
# Overrides (defaults in parens): KLITED_PORT (7443), ETCD_NAME_PREFIX (etcd),
# ETCD_PORT_BASE (2379), ETCD_NET (klite-etcd), KLITE_NODE_PREFIX (node),
# KLITE_NODE_COUNT (4), KLITE_CLUSTER_TOKEN (dev-token), KLITE_DEV_DIR (~/.klite/dev),
# KLITE_DEV_SKIP_BUILD (unset; set to 1 to reuse existing bin/ binaries).
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null 2>&1 || export PATH="/opt/homebrew/bin:$PATH"

KLITED_PORT="${KLITED_PORT:-7443}"
export ETCD_NAME_PREFIX="${ETCD_NAME_PREFIX:-etcd}"
export ETCD_PORT_BASE="${ETCD_PORT_BASE:-2379}"
export ETCD_NET="${ETCD_NET:-klite-etcd}"
KLITE_NODE_PREFIX="${KLITE_NODE_PREFIX:-node}"
KLITE_NODE_COUNT="${KLITE_NODE_COUNT:-4}"
KLITE_CLUSTER_TOKEN="${KLITE_CLUSTER_TOKEN:-dev-token}"
DEV_DIR="${KLITE_DEV_DIR:-$HOME/.klite/dev}"
SKIP_BUILD="${KLITE_DEV_SKIP_BUILD:-}"

BIN=bin
KLITE="$BIN/klite"
export KLITE_SERVER="127.0.0.1:$KLITED_PORT"
ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT_BASE,127.0.0.1:$((ETCD_PORT_BASE + 2)),127.0.0.1:$((ETCD_PORT_BASE + 4))"
NODES=()
for i in $(seq 1 "$KLITE_NODE_COUNT"); do NODES+=("$KLITE_NODE_PREFIX-$i"); done

say() { echo "==> $*"; }
die() { echo "dev-up: $*" >&2; exit 1; }

# wait_for <seconds> <fn>: retries fn every 0.5s within the budget.
wait_for() {
  local budget=$1 fn=$2
  for _ in $(seq 1 $((budget * 2))); do
    "$fn" && return 0
    sleep 0.5
  done
  return 1
}

klited_ready() { "$KLITE" get workloads >/dev/null 2>&1; }
nodes_ready() {
  [[ "$("$KLITE" get nodes 2>/dev/null | awk 'NR>1 && $2=="Ready"' | wc -l | tr -d ' ')" == "$KLITE_NODE_COUNT" ]]
}
instances_running() {
  local rows
  rows="$("$KLITE" get instances 2>/dev/null | awk 'NR>1 && NF>0')"
  # Ready is the steady phase once probes confirm (M4), while Running covers
  # apps without probe targets.
  [[ -n "$rows" ]] && [[ -z "$(awk '$4!="Running" && $4!="Ready"' <<<"$rows")" ]]
}

# --- idempotency: tear down this profile's prior processes and containers ---
say "clearing any previous playground for this profile"
hack/dev-down.sh >/dev/null 2>&1 || true

# --- build ---
if [[ -z "$SKIP_BUILD" ]]; then
  say "building klited, klite, klite-agent"
  go build -o "$BIN/klited" ./cmd/klited
  go build -o "$BIN/klite" ./cmd/klite
  go build -o "$BIN/klite-agent" ./cmd/klite-agent
  if [[ -f build/klite-net.Dockerfile ]]; then
    say "building klite-net:dev image"
    make net-image >/dev/null 2>&1 || echo "note: make net-image failed, continuing without it"
  fi
else
  say "KLITE_DEV_SKIP_BUILD set, using existing bin/ binaries"
  for b in klited klite klite-agent; do
    [[ -x "$BIN/$b" ]] || die "missing $BIN/$b (unset KLITE_DEV_SKIP_BUILD to build)"
  done
fi
docker image inspect klite-net:dev >/dev/null 2>&1 \
  || echo "note: klite-net:dev image missing, so per-node net/envoy infra is skipped (run make net-image to enable it)"

mkdir -p "$DEV_DIR"

# --- etcd ---
say "starting etcd ($ETCD_NAME_PREFIX-1..3 on 127.0.0.1:$ETCD_PORT_BASE/+2/+4)"
hack/etcd-up.sh

# --- klited ---
KLITED_LOG="$DEV_DIR/klited-$KLITED_PORT.log"
KLITED_PIDFILE="$DEV_DIR/klited-$KLITED_PORT.pid"
say "starting klited on 127.0.0.1:$KLITED_PORT (log: $KLITED_LOG)"
"$BIN/klited" --listen "127.0.0.1:$KLITED_PORT" --etcd "$ETCD_ENDPOINTS" \
  --cluster-token "$KLITE_CLUSTER_TOKEN" >"$KLITED_LOG" 2>&1 &
echo $! >"$KLITED_PIDFILE"
disown $! 2>/dev/null || true
wait_for 15 klited_ready || die "klited not answering on $KLITED_PORT (see $KLITED_LOG)"
TOKEN="$("$KLITE" node token)" || die "mint join token"

# --- nodes + agents ---
say "registering nodes: ${NODES[*]}"
for n in "${NODES[@]}"; do
  if [[ -f "examples/seed/nodes/$n.yaml" ]]; then
    "$KLITE" apply -f "examples/seed/nodes/$n.yaml" >/dev/null
  else
    printf 'apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: %s\n  labels:\n    zone: local\nspec:\n  maxInstances: 32\n' "$n" \
      | "$KLITE" apply -f - >/dev/null
  fi
done

for n in "${NODES[@]}"; do
  say "starting agent for $n (log: $DEV_DIR/agent-$n.log)"
  "$BIN/klite-agent" --node "$n" --server "127.0.0.1:$KLITED_PORT" \
    --token "$TOKEN" >"$DEV_DIR/agent-$n.log" 2>&1 &
  echo $! >"$DEV_DIR/agent-$n.pid"
  disown $! 2>/dev/null || true
done
wait_for 30 nodes_ready || die "not all $KLITE_NODE_COUNT nodes Ready (try: $KLITE get nodes; logs in $DEV_DIR)"
say "all $KLITE_NODE_COUNT nodes Ready"
# A reused etcd store can carry cordons from a prior drain run, and the
# playground wants every declared node schedulable.
for n in "${NODES[@]}"; do "$KLITE" uncordon "$n" >/dev/null 2>&1 || true; done

# --- example apps ---
say "applying examples/seed/apps"
"$KLITE" apply -f examples/seed/apps >/dev/null
wait_for 90 instances_running || die "instances not all Running (try: $KLITE get instances; logs in $DEV_DIR)"
say "all instances Running"

# --- cheat sheet ---
A_INST="$("$KLITE" get instances 2>/dev/null | awk 'NR>1 && $2=="a" {print $1; exit}')"
A_INST="${A_INST:-<instance>}"
echo
echo "playground is up"
echo
echo "processes:"
echo "  klited      pid $(cat "$KLITED_PIDFILE")  127.0.0.1:$KLITED_PORT  log $KLITED_LOG"
for n in "${NODES[@]}"; do
  echo "  agent $n  pid $(cat "$DEV_DIR/agent-$n.pid")  log $DEV_DIR/agent-$n.log"
done
echo "  etcd        containers $ETCD_NAME_PREFIX-1..3 on 127.0.0.1:$ETCD_PORT_BASE/$((ETCD_PORT_BASE + 2))/$((ETCD_PORT_BASE + 4))"
echo
echo "poke it:"
echo "  export KLITE_SERVER=127.0.0.1:$KLITED_PORT"
echo "  $KLITE get nodes"
echo "  $KLITE get workloads"
echo "  $KLITE get instances --watch"
echo "  $KLITE logs -f $A_INST     # a's chatter: '-> b ok' on 2.5% rolls"
echo "  $KLITE describe instance $A_INST"
echo "  $KLITE describe workload b"
echo "  $KLITE scale workload b --replicas 5"
echo "  $KLITE apply -f examples/demo-policies/allow-only-a-to-b.yaml"
echo "  $KLITE delete -f examples/demo-policies/allow-only-a-to-b.yaml"
echo "  docker ps --filter label=io.klite.role=workload"
echo "  tail -f $KLITED_LOG"
echo
echo "tear down: hack/dev-down.sh (add --all to stop etcd and remove klite0)"
