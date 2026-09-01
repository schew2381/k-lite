#!/bin/sh
# join.sh turns a fresh Linux box into a k-lite node (ADR 0038). It installs
# Docker if asked, downloads the matching klite-agent release binary, and
# leaves a systemd unit restarting the agent forever. `klite node add <name>`
# prints the exact line to paste:
#
#   curl -sfL https://github.com/schew2381/k-lite/releases/latest/download/join.sh | \
#     KLITE_URL=203.0.113.7:7443 KLITE_TOKEN='K10...' KLITE_NODE=node-4 sh -
#
# Inputs, as environment variables or flags:
#   KLITE_URL        --url        klited address the agent dials (required)
#   KLITE_TOKEN      --token      join token from `klite node token` (required)
#   KLITE_NODE       --node       declared node name (required)
#   KLITE_ADVERTISE  --advertise  address other nodes dial for this node's
#                                 ingress ports; defaults to the detected
#                                 public IPv4, and detection refuses private
#                                 addresses rather than guessing
#   KLITE_VERSION    --version    release tag to install (default: latest)
#   KLITE_YES=1      --yes        consent to installing Docker via get.docker.com
#
# Everything below is wrapped in functions and dispatched from the last line,
# so a truncated download runs nothing.
set -eu

REPO="schew2381/k-lite"
AGENT_BIN="/usr/local/bin/klite-agent"
CONF_DIR="/etc/klite"
UNIT_FILE="/etc/systemd/system/klite-agent.service"
STATE_DIR="/var/lib/klite"

info() { echo "join: $*"; }

fatal() {
    echo "join: error: $*" >&2
    exit 1
}

usage() {
    sed -n '2,23p' "$0" 2>/dev/null || info "see the header of join.sh for usage"
    exit "${1:-0}"
}

as_root() {
    if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi
}

parse_flags() {
    while [ $# -gt 0 ]; do
        case "$1" in
        --url) [ $# -ge 2 ] || fatal "--url needs a value"; KLITE_URL="$2"; shift 2 ;;
        --token) [ $# -ge 2 ] || fatal "--token needs a value"; KLITE_TOKEN="$2"; shift 2 ;;
        --node) [ $# -ge 2 ] || fatal "--node needs a value"; KLITE_NODE="$2"; shift 2 ;;
        --advertise) [ $# -ge 2 ] || fatal "--advertise needs a value"; KLITE_ADVERTISE="$2"; shift 2 ;;
        --version) [ $# -ge 2 ] || fatal "--version needs a value"; KLITE_VERSION="$2"; shift 2 ;;
        --yes | -y) KLITE_YES=1; shift ;;
        --help | -h) usage 0 ;;
        *) fatal "unknown flag $1 (--help lists them)" ;;
        esac
    done
    [ -n "${KLITE_URL:-}" ] || fatal "KLITE_URL (or --url) is required: the klited address the agent dials"
    [ -n "${KLITE_TOKEN:-}" ] || fatal "KLITE_TOKEN (or --token) is required: mint one with 'klite node token'"
    [ -n "${KLITE_NODE:-}" ] || fatal "KLITE_NODE (or --node) is required: the name from 'klite node add'"
}

check_platform() {
    [ "$(uname -s)" = "Linux" ] || fatal "this script targets Linux with systemd. On other machines, copy the
right klite-agent release binary over and run it by hand:
  sudo ./klite-agent --node $KLITE_NODE --server $KLITE_URL --token '...' --advertise-address <routable address>"
    command -v systemctl >/dev/null 2>&1 || fatal "systemd not found; run klite-agent under your own supervisor instead"
    command -v curl >/dev/null 2>&1 || fatal "curl is required"
    if [ "$(id -u)" -ne 0 ]; then
        command -v sudo >/dev/null 2>&1 || fatal "run as root, or install sudo"
    fi
}

ensure_docker() {
    if command -v docker >/dev/null 2>&1; then
        as_root docker info >/dev/null 2>&1 ||
            fatal "docker is installed but the daemon is not answering; start it (systemctl start docker) and re-run"
        return
    fi
    if [ "${KLITE_YES:-0}" = "1" ]; then
        info "installing Docker via get.docker.com"
        curl -fsSL https://get.docker.com | as_root sh -
        as_root docker info >/dev/null 2>&1 || fatal "Docker installed but its daemon is not answering"
        return
    fi
    fatal "docker is missing. Install it yourself:
  curl -fsSL https://get.docker.com | sudo sh -
or re-run this script with KLITE_YES=1 to consent to that exact command."
}

is_ipv4() {
    echo "$1" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
}

# RFC1918, loopback, link-local, and CGNAT (100.64/10, tailnets live there).
# The Docker bridge gateway 172.17.0.1 falls in the 172.16/12 arm, and that
# address is precisely the trap: advertised, it makes every remote node dial
# its own bridge.
is_private_ipv4() {
    case "$1" in
    10.* | 192.168.* | 127.* | 169.254.*) return 0 ;;
    172.1[6-9].* | 172.2[0-9].* | 172.3[01].*) return 0 ;;
    100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*) return 0 ;;
    esac
    return 1
}

pick_advertise() {
    if [ -n "${KLITE_ADVERTISE:-}" ]; then
        info "advertising $KLITE_ADVERTISE (from KLITE_ADVERTISE)"
        return
    fi
    addr="$(curl -4 -sf --max-time 10 https://ifconfig.me 2>/dev/null || true)"
    if ! is_ipv4 "$addr"; then
        addr="$(curl -4 -sf --max-time 10 https://api.ipify.org 2>/dev/null || true)"
    fi
    if ! is_ipv4 "$addr"; then
        addr="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}' | head -n 1)"
        fatal "could not detect a public IPv4 (the local route answers ${addr:-nothing}).
Set KLITE_ADVERTISE to the address other nodes dial: the public IP behind
your port forward, the LAN IP on a LAN, the tailnet IP on a tailnet."
    fi
    if is_private_ipv4 "$addr"; then
        fatal "detected $addr, which is private and unreachable from other nodes.
Set KLITE_ADVERTISE to the address other nodes actually dial. Never use
the Docker bridge gateway (172.17.0.1): every remote node would dial itself."
    fi
    KLITE_ADVERTISE="$addr"
    info "advertising $addr (detected public IPv4; override with KLITE_ADVERTISE)"
}

download_agent() {
    arch="$(uname -m)"
    case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fatal "unsupported architecture $arch (releases cover amd64 and arm64)" ;;
    esac
    if [ -n "${KLITE_VERSION:-}" ]; then
        url="https://github.com/$REPO/releases/download/$KLITE_VERSION/klite-agent-linux-$arch"
    else
        url="https://github.com/$REPO/releases/latest/download/klite-agent-linux-$arch"
    fi
    info "downloading $url"
    if ! curl -fL --max-time 300 -o "$TMP/klite-agent" "$url"; then
        fatal "download failed: $url
The likely cause: the repo (or its releases) is still private, and release
assets on a private repo need credentials this script does not carry. Also
possible: no release has been published yet, or KLITE_VERSION names a tag
that does not exist. Until a public release exists, clone the repo on this
box and 'go build ./cmd/klite-agent' instead."
    fi
    # rm first: install over a running binary would fail with 'text file busy'.
    as_root rm -f "$AGENT_BIN"
    as_root install -m 0755 "$TMP/klite-agent" "$AGENT_BIN"
    info "installed $AGENT_BIN"
}

write_config() {
    as_root install -d -m 0755 "$CONF_DIR"
    cat >"$TMP/agent.env" <<EOF
KLITE_NODE=$KLITE_NODE
KLITE_URL=$KLITE_URL
KLITE_TOKEN=$KLITE_TOKEN
KLITE_ADVERTISE=$KLITE_ADVERTISE
EOF
    # 0600: the join token lives in here.
    as_root install -m 0600 "$TMP/agent.env" "$CONF_DIR/agent.env"

    # Restart=always is safe because restarts are boring by design: the node
    # identity persists under the state dir and SIGTERM leaves containers
    # running for the next run to adopt (docs/real-nodes.md).
    cat >"$TMP/klite-agent.service" <<'EOF'
[Unit]
Description=k-lite node agent
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service

[Service]
EnvironmentFile=/etc/klite/agent.env
ExecStart=/usr/local/bin/klite-agent \
  --node ${KLITE_NODE} \
  --server ${KLITE_URL} \
  --token ${KLITE_TOKEN} \
  --advertise-address ${KLITE_ADVERTISE} \
  --state-dir /var/lib/klite
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    as_root install -m 0644 "$TMP/klite-agent.service" "$UNIT_FILE"
    as_root install -d -m 0755 "$STATE_DIR"
    info "wrote $CONF_DIR/agent.env and $UNIT_FILE"
}

start_service() {
    as_root systemctl daemon-reload
    as_root systemctl enable klite-agent >/dev/null 2>&1
    as_root systemctl restart klite-agent
    info "klite-agent enabled and started as node $KLITE_NODE"
    info "watch from the control plane: klite get nodes -w"
}

main() {
    parse_flags "$@"
    check_platform
    TMP="$(mktemp -d)"
    trap 'rm -rf "$TMP"' EXIT
    ensure_docker
    pick_advertise
    download_agent
    write_config
    start_service
}

main "$@"
