#!/usr/bin/env bash
#
# Spike 1: prove the Docker networking facts the k-lite data path rests on,
# against the local Docker daemon (colima).
#
# Checks, in run order:
#   network    a user-defined bridge (klite0, 10.44.0.0/16) accepts static
#              container IPs outside its dynamic --ip-range
#   server     one container can hold an extra /32 on eth0 (a service VIP,
#              10.44.64.7) and answer DNS and HTTP on it
#   dns-vip    Docker's embedded DNS (127.0.0.11) forwards names it doesn't
#              own to the --dns upstream, here a dnsmasq running in a
#              container on the same bridge
#   http-name  --dns-search lets a client reach the VIP by bare name
#              (wget http://b:8080)
#   upstream   names outside our zone still resolve through the same chain
#              (client -> embedded DNS -> dnsmasq -> 1.1.1.1)
#   host-vip   informational: the macOS host can't reach the VIP, because
#              the bridge exists only inside the colima VM
#
# Usage:
#   hack/spike-net.sh        run all checks (removes leftover spike
#                            containers first, leaves klite0 behind)
#   hack/spike-net.sh down   remove the spike containers and klite0, then exit
#
# Exit status: 0 when every required check passes, 1 otherwise.

set -u

NET=klite0
SUBNET=10.44.0.0/16
GATEWAY=10.44.0.1
IP_RANGE=10.44.128.0/17
NET_LABEL=io.klite.role=spike

IMAGE=alpine:3.20
SERVER=spike-b
CLIENT=spike-a
SERVER_IP=10.44.0.10
VIP=10.44.64.7
FQDN=b.svc.klite
SEARCH=svc.klite
BODY=spike-ok
READY_TIMEOUT=90

RESULTS=()
FAILED=0
CLIENT_RESOLV=""

log() { printf '>>> %s\n' "$*"; }

# record <PASS|FAIL|INFO> <check> <detail>
record() {
  RESULTS+=("$(printf '%-4s  %-9s  %s' "$1" "$2" "$3")")
  if [ "$1" = FAIL ]; then FAILED=1; fi
  printf '%s  %s: %s\n' "$1" "$2" "$3"
}

rm_containers() {
  docker rm -f "$SERVER" "$CLIENT" >/dev/null 2>&1 || true
}

# Print the summary, remove the containers, keep the network, and exit.
finish() {
  rm_containers
  echo
  echo "== summary =="
  local line
  for line in "${RESULTS[@]}"; do printf '  %s\n' "$line"; done
  if [ -n "$CLIENT_RESOLV" ]; then
    echo
    echo "client /etc/resolv.conf:"
    printf '%s\n' "$CLIENT_RESOLV" | sed 's/^/  | /'
  fi
  echo
  if [ "$FAILED" -eq 0 ]; then
    echo "RESULT: PASS  (network $NET left in place, '$0 down' removes it)"
  else
    echo "RESULT: FAIL"
  fi
  exit "$FAILED"
}

if [ "${1:-}" = down ]; then
  rm_containers
  docker network rm "$NET" >/dev/null 2>&1 || true
  log "removed $CLIENT, $SERVER, and $NET (where present)"
  exit 0
fi

command -v docker >/dev/null 2>&1 || { echo "docker CLI not found" >&2; exit 2; }
docker version >/dev/null 2>&1 || { echo "docker daemon unreachable (is colima running?)" >&2; exit 2; }
log "daemon: $(docker version --format '{{.Server.Version}} ({{.Server.Os}}/{{.Server.Arch}})' 2>/dev/null), context: $(docker context show 2>/dev/null)"

rm_containers # leftovers from a previous run

# --- network: create klite0 unless a matching one already exists ------------
if docker network inspect "$NET" >/dev/null 2>&1; then
  have=$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "$NET")
  if [ "$have" = "$SUBNET" ]; then
    record PASS network "reusing existing $NET ($have)"
  elif docker network rm "$NET" >/dev/null 2>&1 &&
       docker network create --subnet "$SUBNET" --gateway "$GATEWAY" \
         --ip-range "$IP_RANGE" --label "$NET_LABEL" "$NET" >/dev/null; then
    record PASS network "recreated $NET (had subnet $have, want $SUBNET)"
  else
    record FAIL network "$NET exists with subnet $have and couldn't be replaced"
  fi
elif docker network create --subnet "$SUBNET" --gateway "$GATEWAY" \
       --ip-range "$IP_RANGE" --label "$NET_LABEL" "$NET" >/dev/null; then
  record PASS network "created $NET ($SUBNET, gw $GATEWAY, dynamic range $IP_RANGE)"
else
  record FAIL network "docker network create $NET failed"
fi
[ "$FAILED" -eq 0 ] || finish

docker image inspect "$IMAGE" >/dev/null 2>&1 || { log "pulling $IMAGE"; docker pull "$IMAGE" >/dev/null; }

# --- server: static IP, VIP on eth0, dnsmasq for the zone, HTTP on the VIP --
# dnsmasq answers $FQDN (and subdomains) with the VIP and forwards everything
# else to 1.1.1.1. socat answers HTTP on the VIP with a fixed 200. The
# response lives in a file because socat runs its own quote and backslash
# parsing on SYSTEM: commands, which mangles an inline printf.
SERVER_CMD=$(cat <<'EOF'
set -e
apk add --no-cache dnsmasq socat >/dev/null
printf 'HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nspike-ok\n' > /http200
ip addr add 10.44.64.7/32 dev eth0
dnsmasq -k --port=53 --no-resolv --server=1.1.1.1 --address=/b.svc.klite/10.44.64.7 &
exec socat TCP-LISTEN:8080,bind=10.44.64.7,fork,reuseaddr SYSTEM:'cat /http200'
EOF
)
log "starting $SERVER ($SERVER_IP, VIP $VIP)"
docker run -d --name "$SERVER" --network "$NET" --ip "$SERVER_IP" \
  --cap-add NET_ADMIN "$IMAGE" sh -c "$SERVER_CMD" >/dev/null

# Poll from inside the server until dnsmasq answers on 127.0.0.1 and socat
# serves the expected body on the VIP. apk install time dominates here.
ready=0
deadline=$(( $(date +%s) + READY_TIMEOUT ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if [ "$(docker inspect -f '{{.State.Running}}' "$SERVER" 2>/dev/null)" != "true" ]; then
    break # container died (logs are printed below)
  fi
  if docker exec "$SERVER" sh -c \
       "nslookup $FQDN 127.0.0.1 >/dev/null 2>&1 && wget -qO- -T 2 http://$VIP:8080 2>/dev/null | grep -q $BODY"; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" -eq 1 ]; then
  record PASS server "$SERVER answers DNS on :53 and HTTP on $VIP:8080"
else
  record FAIL server "$SERVER not ready after ${READY_TIMEOUT}s (docker logs follow)"
  docker logs "$SERVER" 2>&1 | tail -20 | sed 's/^/  [logs] /'
  finish
fi

# --- client: embedded DNS with our dnsmasq as upstream, search domain -------
# Docker writes "options ndots:0" by default, and musl (the libc in Alpine)
# skips the search list whenever a name has at least ndots dots. A bare "b"
# would be queried literally and never as b.svc.klite. With ndots:1,
# single-label names go through the search list first.
log "starting $CLIENT (--dns $SERVER_IP --dns-search $SEARCH --dns-opt ndots:1)"
docker run -d --name "$CLIENT" --network "$NET" \
  --dns "$SERVER_IP" --dns-search "$SEARCH" --dns-opt ndots:1 \
  "$IMAGE" sleep 600 >/dev/null
CLIENT_RESOLV=$(docker exec "$CLIENT" cat /etc/resolv.conf)

# dns-vip: a name in our zone must come back as the VIP.
out=$(docker exec "$CLIENT" nslookup "$FQDN" 2>&1)
if printf '%s' "$out" | grep -q "$VIP"; then
  record PASS dns-vip "nslookup $FQDN -> $VIP via embedded DNS"
else
  record FAIL dns-vip "nslookup $FQDN didn't return $VIP"
  printf '%s\n' "$out" | sed 's/^/  [nslookup] /'
fi

# http-name: a bare name plus the search domain reaches the VIP end to end.
out=$(docker exec "$CLIENT" wget -qO- -T 5 http://b:8080 2>&1)
if [ "$out" = "$BODY" ]; then
  record PASS http-name "wget http://b:8080 -> $BODY"
else
  record FAIL http-name "wget http://b:8080 returned '$out'"
fi

# upstream: names outside the zone must still resolve. A fetched page is a
# bonus, and DNS alone is enough (colima guests sometimes lack HTTP egress).
if docker exec "$CLIENT" wget -qO- -T 5 http://example.com >/dev/null 2>&1; then
  record PASS upstream "example.com resolves and HTTP fetch works"
elif docker exec "$CLIENT" nslookup example.com >/dev/null 2>&1; then
  record PASS upstream "example.com resolves but HTTP fetch failed (egress blocked?)"
else
  record FAIL upstream "example.com doesn't resolve from $CLIENT"
  docker exec "$CLIENT" nslookup example.com 2>&1 | sed 's/^/  [nslookup] /'
fi

# host-vip is informational only. The design assumes container addresses
# aren't reachable from macOS, so record whatever this machine actually does.
hostbody=$(curl -m 2 -s "http://$VIP:8080" 2>/dev/null)
rc=$?
if [ "$rc" -eq 0 ] && [ "$hostbody" = "$BODY" ]; then
  record INFO host-vip "UNEXPECTED: macOS host CAN reach $VIP:8080 (host/bridge boundary doesn't hold here)"
else
  record INFO host-vip "macOS host can't reach $VIP:8080 (curl exit $rc), as the design assumes"
fi

finish
