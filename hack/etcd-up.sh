#!/usr/bin/env bash
# Run (or tear down) the 3-member etcd cluster k-lite stores state in.
# Client endpoints land on 127.0.0.1:2379, :2381, :2383.
set -euo pipefail

IMG="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
NET=klite-etcd
DATA_ROOT="${HOME}/.klite/etcd"

if [[ "${1:-up}" == "down" ]]; then
  docker rm -f etcd-1 etcd-2 etcd-3 2>/dev/null || true
  docker network rm "$NET" 2>/dev/null || true
  exit 0
fi

docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET"

CLUSTER="etcd-1=http://etcd-1:2380,etcd-2=http://etcd-2:2380,etcd-3=http://etcd-3:2380"

for i in 1 2 3; do
  port=$((2379 + (i - 1) * 2))
  docker rm -f "etcd-$i" 2>/dev/null || true
  mkdir -p "$DATA_ROOT/etcd-$i"
  docker run -d --name "etcd-$i" --network "$NET" \
    --label io.klite.role=etcd \
    -p "127.0.0.1:$port:2379" \
    -e ETCD_UNSUPPORTED_ARCH=arm64 \
    -v "$DATA_ROOT/etcd-$i:/etcd-data" \
    "$IMG" etcd \
    --name "etcd-$i" \
    --data-dir /etcd-data \
    --initial-advertise-peer-urls "http://etcd-$i:2380" \
    --listen-peer-urls http://0.0.0.0:2380 \
    --listen-client-urls http://0.0.0.0:2379 \
    --advertise-client-urls "http://etcd-$i:2379" \
    --initial-cluster "$CLUSTER" \
    --initial-cluster-state new \
    --initial-cluster-token klite-etcd \
    --auto-compaction-retention 1h >/dev/null
done

echo "waiting for etcd quorum..."
for _ in $(seq 1 30); do
  if docker exec etcd-1 etcdctl endpoint health --cluster >/dev/null 2>&1; then
    echo "etcd healthy on 127.0.0.1:2379 / :2381 / :2383"
    exit 0
  fi
  sleep 1
done
echo "etcd did not become healthy within 30s" >&2
docker logs etcd-1 | tail -20 >&2
exit 1
