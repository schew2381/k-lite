#!/usr/bin/env bash
# Spike 2 for ADR 0007: prove that an Envoy container joined to another
# container's network namespace can take LDS/CDS/EDS from a control plane on
# the macOS host, bind a VIP listener via freebind, proxy TCP, and apply an
# RBAC filter swap without dropping bystander traffic.
#
# Usage:
#   ./run.sh        run all three phases, exit nonzero on any failure
#   ./run.sh down   remove spike containers and the xDS server
#
# The klite0 network and the images are always left in place.

set -u

SPIKE_DIR="$(cd "$(dirname "$0")" && pwd)"
GO="${GO:-/opt/homebrew/bin/go}"
ENVOY_IMAGE="envoyproxy/envoy:v1.31.5"
NET="klite0"
NETNS_IP="10.44.0.20"
VIP="10.44.64.7"
VIP_PORT="8080"
XDS_PORT="18000"
PIPE="$SPIKE_DIR/.phase-pipe"
PIDFILE="$SPIKE_DIR/xds-server.pid"
SERVER_LOG="$SPIKE_DIR/xds-server.log"
HAMMER_LOG="$SPIKE_DIR/.hammer.log"
CONTAINERS="spike-envoy spike-addhost-probe spike-netns spike-client spike-client2 spike-whoami"

RESULTS=""
FAILED=0

say()  { printf '\n=== %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }

record() { # record <check> <PASS|FAIL> <detail>
  local check="$1" verdict="$2"
  shift 2
  RESULTS="${RESULTS}
  ${check}: ${verdict} (${*})"
  [ "$verdict" = "FAIL" ] && FAILED=1
  printf '>>> %s: %s (%s)\n' "$check" "$verdict" "$*"
}

kill_server() {
  if [ -f "$PIDFILE" ]; then
    kill "$(cat "$PIDFILE")" >/dev/null 2>&1
    rm -f "$PIDFILE"
  fi
  pkill -f "$SPIKE_DIR/xds-server" >/dev/null 2>&1
  return 0
}

down() {
  docker rm -f $CONTAINERS >/dev/null 2>&1
  kill_server
  rm -f "$PIPE" "$HAMMER_LOG"
  return 0
}

if [ "${1:-}" = "down" ]; then
  down
  echo "spike containers and xDS server removed ($NET network and images kept)"
  exit 0
fi

ip_of()      { docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$1"; }
admin_get()  { # GET an Envoy admin path; curl inside spike-envoy, wget in the shared netns as fallback
  docker exec spike-envoy curl -sf "http://127.0.0.1:9901$1" 2>/dev/null \
    || docker exec spike-netns wget -qO- -T 2 "http://127.0.0.1:9901$1" 2>/dev/null
}
stat_value() { admin_get /stats | grep "^$1: " | head -1 | awk '{print $2}'; }
client_get() { docker exec "$1" wget -qO- -T 5 "http://$VIP:$VIP_PORT" 2>/dev/null; }

start_stack() { # start_stack <ip-or-host-gateway>: recreate spike-netns and spike-envoy
  docker rm -f spike-envoy spike-netns >/dev/null 2>&1
  # The hosts entry lives on the netns donor: a container joining another
  # container's network namespace inherits the donor's /etc/hosts, and docker
  # rejects --add-host together with --network container:<name>.
  docker run -d --name spike-netns --network "$NET" --ip "$NETNS_IP" \
    --cap-add NET_ADMIN --add-host "host.docker.internal:$1" \
    alpine:3.20 sleep infinity >/dev/null || return 1
  docker run -d --name spike-envoy --network "container:spike-netns" \
    -v "$SPIKE_DIR/bootstrap.yaml:/bootstrap.yaml:ro" \
    "$ENVOY_IMAGE" -c /bootstrap.yaml >/dev/null || return 1
}

wait_connected() { # true once control_plane.connected_state reports a live ADS stream
  local i v
  for i in $(seq 1 20); do
    v="$(stat_value control_plane.connected_state)"
    [ "${v:-}" = "1" ] && return 0
    sleep 0.5
  done
  return 1
}

say "spike 2: Envoy data plane on docker context '$(docker context show)'"
down

# --- network ---------------------------------------------------------------
if docker network inspect "$NET" >/dev/null 2>&1; then
  note "network $NET already exists"
else
  docker network create --subnet 10.44.0.0/16 --ip-range 10.44.128.0/17 "$NET" >/dev/null || exit 1
  note "created network $NET (subnet 10.44.0.0/16, ip-range 10.44.128.0/17)"
fi

# --- backend and clients ---------------------------------------------------
say "starting backend and client containers"
docker run -d --name spike-whoami --network "$NET" traefik/whoami:v1.10 >/dev/null || exit 1
docker run -d --name spike-client --network "$NET" alpine:3.20 sleep infinity >/dev/null || exit 1
docker run -d --name spike-client2 --network "$NET" alpine:3.20 sleep infinity >/dev/null || exit 1
WHOAMI_IP="$(ip_of spike-whoami)"
CLIENT_IP="$(ip_of spike-client)"
CLIENT2_IP="$(ip_of spike-client2)"
note "whoami=$WHOAMI_IP client=$CLIENT_IP client2=$CLIENT2_IP"
if [ -z "$WHOAMI_IP" ] || [ -z "$CLIENT_IP" ] || [ -z "$CLIENT2_IP" ]; then
  echo "FATAL: could not read container IPs" >&2
  exit 1
fi

# --- xDS server on the host --------------------------------------------------
say "building and starting the xDS server on the host"
"$GO" build -C "$SPIKE_DIR" -o xds-server . || { echo "FATAL: go build failed" >&2; exit 1; }
rm -f "$PIPE"
mkfifo "$PIPE"
"$SPIKE_DIR/xds-server" -backend "$WHOAMI_IP:80" -client-ip "$CLIENT_IP" \
  <"$PIPE" >"$SERVER_LOG" 2>&1 &
echo $! >"$PIDFILE"
exec 3>"$PIPE" # keep the write end open; the server exits on stdin EOF
XDS_UP=0
for i in $(seq 1 20); do
  nc -z 127.0.0.1 "$XDS_PORT" >/dev/null 2>&1 && { XDS_UP=1; break; }
  sleep 0.5
done
if [ "$XDS_UP" != 1 ]; then
  echo "FATAL: xDS server not listening on :$XDS_PORT; log tail:" >&2
  tail -20 "$SERVER_LOG" >&2
  exit 1
fi
note "xDS server on 0.0.0.0:$XDS_PORT (pid $(cat "$PIDFILE"), log: $SERVER_LOG)"

# --- Envoy in the donor netns, probing host reachability --------------------
say "starting Envoy in spike-netns's network namespace"
XDS_REACH=""
RESOLVED=""
for CAND in host-gateway 192.168.5.2; do
  note "trying host.docker.internal -> $CAND"
  start_stack "$CAND" || { echo "FATAL: docker run failed" >&2; exit 1; }
  RESOLVED="$(docker exec spike-netns awk '/host\.docker\.internal/ {ip=$1} END {print ip}' /etc/hosts)"
  note "resolves to ${RESOLVED:-<nothing>} inside the netns"
  if wait_connected; then
    XDS_REACH="$CAND ($RESOLVED)"
    break
  fi
  note "no ADS stream via $CAND; envoy log tail:"
  docker logs spike-envoy 2>&1 | tail -3 | sed 's/^/      /'
done
if [ -z "$XDS_REACH" ]; then
  record "xds-reachability" FAIL "no route from Envoy to host:$XDS_PORT via host-gateway or 192.168.5.2"
else
  record "xds-reachability" PASS "host.docker.internal -> $XDS_REACH"
fi

# Record what docker itself says about the spec-literal form (--add-host on
# the joining container); informational only.
ADDHOST_PROBE="$(docker create --name spike-addhost-probe --network "container:spike-netns" \
  --add-host host.docker.internal:host-gateway "$ENVOY_IMAGE" -c /bootstrap.yaml 2>&1)"
ADDHOST_RC=$?
docker rm -f spike-addhost-probe >/dev/null 2>&1
if [ "$ADDHOST_RC" = 0 ]; then
  note "docker accepts --add-host with --network container: (probe container created fine)"
else
  note "docker rejects --add-host with --network container:: $(echo "$ADDHOST_PROBE" | tail -1)"
fi

if [ "$FAILED" = 1 ]; then
  say "cannot continue without xDS; server log tail:"
  tail -20 "$SERVER_LOG"
  exit 1
fi

# --- phase 1: dynamic config, freebind, TCP proxy ---------------------------
say "phase 1: LDS/CDS/EDS delivery, freebind bind of $VIP, TCP proxy"
LDS_OK=0
for i in $(seq 1 20); do
  UP="$(stat_value listener_manager.lds.update_success)"
  ACTIVE="$(stat_value listener_manager.total_listeners_active)"
  if [ "${UP:-0}" -ge 1 ] 2>/dev/null && [ "${ACTIVE:-0}" -ge 1 ] 2>/dev/null; then
    LDS_OK=1
    break
  fi
  sleep 0.5
done
if [ "$LDS_OK" = 1 ]; then
  record "phase1-freebind" PASS "listener active before $VIP exists on eth0, so IP_FREEBIND did the bind"
else
  REJ="$(stat_value listener_manager.lds.update_rejected)"
  record "phase1-freebind" FAIL "listener never active (lds.update_rejected=${REJ:-?})"
  docker logs spike-envoy 2>&1 | tail -5 | sed 's/^/      /'
fi

if ! docker exec spike-netns ip addr add "$VIP/32" dev eth0 2>/dev/null; then
  note "busybox ip failed; installing iproute2 in spike-netns"
  docker exec spike-netns apk add --quiet iproute2 >/dev/null 2>&1
  docker exec spike-netns ip addr add "$VIP/32" dev eth0 || note "WARNING: could not add $VIP to eth0"
fi
note "added $VIP/32 to spike-netns eth0"

P1_BODY=""
for i in $(seq 1 10); do
  if P1_BODY="$(client_get spike-client)" && printf '%s' "$P1_BODY" | grep -q "Hostname:"; then
    break
  fi
  P1_BODY=""
  sleep 1
done
if [ -n "$P1_BODY" ]; then
  record "phase1-proxy" PASS "spike-client fetched whoami through $VIP:$VIP_PORT"
  printf '%s\n' "$P1_BODY" | head -3 | sed 's/^/      /'
else
  record "phase1-proxy" FAIL "wget through the VIP returned nothing"
  docker logs spike-envoy 2>&1 | tail -5 | sed 's/^/      /'
fi

if admin_get /config_dump | grep -q '"vip-b"'; then
  record "phase1-config-dump" PASS "admin /config_dump lists listener vip-b"
  admin_get /config_dump | head -8 | sed 's/^/      /'
else
  record "phase1-config-dump" FAIL "listener vip-b missing from /config_dump"
fi

# --- phase 2: RBAC deny for spike-client, hitless for spike-client2 ---------
say "phase 2: RBAC deny for $CLIENT_IP; spike-client2 must keep working throughout"
LISTENER_GEN_BEFORE="$(stat_value listener_manager.listener_modified)"
: >"$HAMMER_LOG"
(
  i=0
  while [ "$i" -lt 40 ]; do
    if BODY="$(client_get spike-client2)" && printf '%s' "$BODY" | grep -q "Hostname:"; then
      :
    else
      echo "miss at request $i" >>"$HAMMER_LOG"
    fi
    i=$((i + 1))
  done
) &
HAMMER_PID=$!

echo 2 >&3

DENIED=0
for i in $(seq 1 30); do
  if ! client_get spike-client >/dev/null 2>&1; then
    DENIED=1
    break
  fi
  sleep 0.5
done
if [ "$DENIED" = 1 ]; then
  DENY_MSG="$(docker exec spike-client wget -O /dev/null -T 5 "http://$VIP:$VIP_PORT" 2>&1 | tail -1)"
  record "phase2-deny" PASS "spike-client now refused: ${DENY_MSG:-connection failed}"
else
  record "phase2-deny" FAIL "spike-client still gets through 15s after the RBAC push"
fi

C2_OK=1
for i in 1 2 3; do
  if BODY="$(client_get spike-client2)" && printf '%s' "$BODY" | grep -q "Hostname:"; then
    :
  else
    C2_OK=0
    break
  fi
done
if [ "$C2_OK" = 1 ]; then
  record "phase2-targeted" PASS "spike-client2 ($CLIENT2_IP) unaffected by the deny"
else
  record "phase2-targeted" FAIL "spike-client2 broken by a deny aimed at $CLIENT_IP"
fi

wait "$HAMMER_PID"
HAMMER_MISSES="$(wc -l <"$HAMMER_LOG" | tr -d ' ')"
LISTENER_GEN_AFTER="$(stat_value listener_manager.listener_modified)"
if [ "$HAMMER_MISSES" = 0 ]; then
  record "phase2-hitless" PASS "40 spike-client2 requests across the listener swap, zero misses (listener_modified ${LISTENER_GEN_BEFORE:-?} -> ${LISTENER_GEN_AFTER:-?})"
else
  record "phase2-hitless" FAIL "$HAMMER_MISSES of 40 spike-client2 requests missed during the swap"
  sed 's/^/      /' "$HAMMER_LOG" | head -5
fi
note "rbac stats: $(admin_get /stats | grep 'rbac' | grep -v shadow | tr '\n' ' ')"

# --- phase 3: RBAC removed, spike-client recovers ----------------------------
say "phase 3: drop the RBAC filter; spike-client must recover"
echo 3 >&3
RECOVERED=0
for i in $(seq 1 30); do
  if BODY="$(client_get spike-client)" && printf '%s' "$BODY" | grep -q "Hostname:"; then
    RECOVERED=1
    break
  fi
  sleep 0.5
done
if [ "$RECOVERED" = 1 ]; then
  record "phase3-recover" PASS "spike-client gets whoami again after the RBAC removal"
else
  record "phase3-recover" FAIL "spike-client still denied 15s after the RBAC removal"
fi

# --- summary -----------------------------------------------------------------
say "results"
printf '%s\n\n' "$RESULTS"
echo "xDS reachability from containers: host.docker.internal -> ${XDS_REACH:-UNREACHABLE}"
if [ "$FAILED" = 0 ]; then
  down
  echo "ALL PHASES PASSED (containers removed; network $NET and images kept)"
  exit 0
fi
echo "SPIKE FAILED; containers and server left running for inspection (./run.sh down to clean up)"
echo "server log tail:"
tail -10 "$SERVER_LOG"
exit 1
