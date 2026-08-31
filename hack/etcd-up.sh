#!/usr/bin/env bash
# Run (or tear down) the 3-member etcd cluster k-lite stores state in.
# Defaults: names etcd-1..3, client endpoints 127.0.0.1:2379, :2381, :2383.
# Overrides: ETCD_NAME_PREFIX, ETCD_PORT_BASE, ETCD_NET (for side-by-side dev clusters).
set -euo pipefail

IMG="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
PREFIX="${ETCD_NAME_PREFIX:-etcd}"
PORT_BASE="${ETCD_PORT_BASE:-2379}"
NET="${ETCD_NET:-klite-etcd}"
DATA_ROOT="${HOME}/.klite/etcd"

if [[ "${1:-up}" == "down" ]]; then
  docker rm -f "$PREFIX-1" "$PREFIX-2" "$PREFIX-3" 2>/dev/null || true
  docker network rm "$NET" 2>/dev/null || true
  exit 0
fi

docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

CLUSTER="$PREFIX-1=http://$PREFIX-1:2380,$PREFIX-2=http://$PREFIX-2:2380,$PREFIX-3=http://$PREFIX-3:2380"

for i in 1 2 3; do
  port=$((PORT_BASE + (i - 1) * 2))
  docker rm -f "$PREFIX-$i" 2>/dev/null || true
  mkdir -p "$DATA_ROOT/$PREFIX-$i"
  docker run -d --name "$PREFIX-$i" --network "$NET" \
    --label io.klite.role=etcd \
    -p "127.0.0.1:$port:2379" \
    -e ETCD_UNSUPPORTED_ARCH=arm64 \
    -v "$DATA_ROOT/$PREFIX-$i:/etcd-data" \
    "$IMG" etcd \
    --name "$PREFIX-$i" \
    --data-dir /etcd-data \
    --initial-advertise-peer-urls "http://$PREFIX-$i:2380" \
    --listen-peer-urls http://0.0.0.0:2380 \
    --listen-client-urls http://0.0.0.0:2379 \
    --advertise-client-urls "http://$PREFIX-$i:2379" \
    --initial-cluster "$CLUSTER" \
    --initial-cluster-state new \
    --initial-cluster-token "$NET" \
    --auto-compaction-retention 1h >/dev/null
done

echo "waiting for etcd quorum..."
for _ in $(seq 1 30); do
  if docker exec "$PREFIX-1" etcdctl endpoint health --cluster >/dev/null 2>&1; then
    echo "etcd healthy on 127.0.0.1:$PORT_BASE / :$((PORT_BASE + 2)) / :$((PORT_BASE + 4))"
    exit 0
  fi
  sleep 1
done
echo "etcd did not become healthy within 30s" >&2
docker logs "$PREFIX-1" | tail -20 >&2
exit 1
