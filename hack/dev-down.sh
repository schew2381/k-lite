#!/usr/bin/env bash
# Tear down the dev playground started by hack/dev-up.sh.
# Kills pidfile processes and removes this profile's workload/infra containers.
# --all additionally stops etcd (honoring ETCD_* overrides) and removes klite0.
# Honors the same env overrides as dev-up.sh.
set -uo pipefail
cd "$(dirname "$0")/.."

KLITE_NODE_PREFIX="${KLITE_NODE_PREFIX:-node}"
KLITE_NODE_COUNT="${KLITE_NODE_COUNT:-3}"
DEV_DIR="${KLITE_DEV_DIR:-$HOME/.klite/dev}"
ALL=0
[[ "${1:-}" == "--all" ]] && ALL=1

# --- kill playground processes (pidfiles only, never by name) ---
for pidfile in "$DEV_DIR"/klited-*.pid "$DEV_DIR"/agent-*.pid; do
  [[ -f "$pidfile" ]] || continue
  pid="$(cat "$pidfile" 2>/dev/null)"
  if [[ -n "$pid" ]] && ps -p "$pid" -o command= 2>/dev/null | grep -Eq 'klited|klite-agent'; then
    echo "stopping pid $pid ($(basename "$pidfile" .pid))"
    kill "$pid" 2>/dev/null
    for _ in $(seq 1 10); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    kill -9 "$pid" 2>/dev/null
  fi
  rm -f "$pidfile"
done

# --- remove this profile's containers by io.klite labels ---
nodes=()
for i in $(seq 1 "$KLITE_NODE_COUNT"); do nodes+=("$KLITE_NODE_PREFIX-$i"); done
if [[ -d "$DEV_DIR" ]]; then
  for f in "$DEV_DIR"/agent-*.log; do
    [[ -f "$f" ]] || continue
    n="$(basename "$f" .log)"; n="${n#agent-}"
    [[ " ${nodes[*]-} " == *" $n "* ]] || nodes+=("$n")
  done
fi
for n in "${nodes[@]}"; do
  ids="$(docker ps -aq --filter "label=io.klite.node=$n" 2>/dev/null)"
  if [[ -n "$ids" ]]; then
    echo "removing containers for node $n"
    xargs docker rm -f >/dev/null 2>&1 <<<"$ids"
  fi
done

if [[ "$ALL" == 1 ]]; then
  echo "stopping etcd and removing klite0"
  hack/etcd-up.sh down
  docker network rm klite0 2>/dev/null || true
fi

echo "dev-down: done"
