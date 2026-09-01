#!/bin/sh
# test-join.sh unit-tests join.sh's decision helpers: address classification,
# the tailnet-IP derivation, and pick_advertise's refusals. A fake tailscale
# binary on PATH stands in for the real one, so nothing here touches Docker,
# systemd, the network, or a real tailnet. Run it directly: sh hack/test-join.sh
set -eu

DIR="$(cd "$(dirname "$0")" && pwd)"
KLITE_JOIN_TEST=1
# shellcheck source=join.sh
. "$DIR/join.sh"

# join.sh escalates through sudo when not root. The fakes don't need it,
# and CI has no tty to prompt on.
as_root() { "$@"; }

FAILS=0
pass() { echo "PASS $1"; }
fail() {
    echo "FAIL $1" >&2
    FAILS=$((FAILS + 1))
}

expect_true() {
    desc="$1"
    shift
    if "$@"; then pass "$desc"; else fail "$desc"; fi
}

expect_false() {
    desc="$1"
    shift
    if "$@"; then fail "$desc"; else pass "$desc"; fi
}

expect_eq() {
    desc="$1"
    got="$2"
    want="$3"
    if [ "$got" = "$want" ]; then pass "$desc"; else fail "$desc: got '$got', want '$want'"; fi
}

# --- is_ipv4 -----------------------------------------------------------------
expect_true "is_ipv4 accepts a dotted quad" is_ipv4 "203.0.113.7"
expect_true "is_ipv4 accepts a tailnet address" is_ipv4 "100.64.1.5"
expect_false "is_ipv4 rejects a hostname" is_ipv4 "example.com"
expect_false "is_ipv4 rejects empty" is_ipv4 ""
expect_false "is_ipv4 rejects host:port" is_ipv4 "203.0.113.7:7443"

# --- is_cgnat_ipv4: the 100.64.0.0/10 boundaries -----------------------------
expect_true "cgnat: 100.64.0.0 (range floor)" is_cgnat_ipv4 "100.64.0.0"
expect_true "cgnat: 100.100.100.100" is_cgnat_ipv4 "100.100.100.100"
expect_true "cgnat: 100.127.255.255 (range ceiling)" is_cgnat_ipv4 "100.127.255.255"
expect_false "cgnat: 100.63.255.255 (below floor)" is_cgnat_ipv4 "100.63.255.255"
expect_false "cgnat: 100.128.0.0 (above ceiling)" is_cgnat_ipv4 "100.128.0.0"
expect_false "cgnat: 10.64.0.1 (RFC1918, not CGNAT)" is_cgnat_ipv4 "10.64.0.1"
expect_false "cgnat: public 8.8.8.8" is_cgnat_ipv4 "8.8.8.8"

# --- is_private_ipv4 ----------------------------------------------------------
expect_true "private: 10.0.0.1" is_private_ipv4 "10.0.0.1"
expect_true "private: 192.168.1.9" is_private_ipv4 "192.168.1.9"
expect_true "private: 127.0.0.1" is_private_ipv4 "127.0.0.1"
expect_true "private: 169.254.9.9" is_private_ipv4 "169.254.9.9"
expect_true "private: 172.16.0.1 (172.16/12 floor)" is_private_ipv4 "172.16.0.1"
expect_true "private: 172.31.255.254 (172.16/12 ceiling)" is_private_ipv4 "172.31.255.254"
expect_true "private: docker bridge gateway 172.17.0.1" is_private_ipv4 "172.17.0.1"
expect_true "private: CGNAT folds in (100.99.5.5)" is_private_ipv4 "100.99.5.5"
expect_false "private: 172.15.0.1 (below 172.16/12)" is_private_ipv4 "172.15.0.1"
expect_false "private: 172.32.0.1 (above 172.16/12)" is_private_ipv4 "172.32.0.1"
expect_false "private: public 8.8.8.8" is_private_ipv4 "8.8.8.8"
expect_false "private: public 100.63.0.1" is_private_ipv4 "100.63.0.1"

# --- url_host -----------------------------------------------------------------
expect_eq "url_host strips a port" "$(url_host "100.64.7.1:7443")" "100.64.7.1"
expect_eq "url_host keeps a bare host" "$(url_host "100.64.7.1")" "100.64.7.1"
expect_eq "url_host handles names" "$(url_host "cp.example.com:7443")" "cp.example.com"

# --- pick_advertise refusals (fatal exits stay inside subshells) ---------------

# Explicit KLITE_ADVERTISE is accepted verbatim, tailnet addresses included.
if (KLITE_ADVERTISE="100.64.9.9" pick_advertise >/dev/null 2>&1); then
    pass "pick_advertise accepts an explicit tailnet address"
else
    fail "pick_advertise accepts an explicit tailnet address"
fi

# A tailnet control-plane URL with no explicit advertise must refuse before
# any detection runs (the guard precedes the curl, so this needs no network).
out="$( (KLITE_URL="100.64.7.1:7443" KLITE_ADVERTISE="" pick_advertise) 2>&1 || true)"
case "$out" in
*KLITE_VPN=tailscale*) pass "tailnet URL without advertise refuses and names the fix" ;;
*) fail "tailnet URL without advertise refuses and names the fix: got '$out'" ;;
esac

# --- ensure_tailscale against a fake binary ------------------------------------
TMP_TEST="$(mktemp -d)"
trap 'rm -rf "$TMP_TEST"' EXIT
MARKER="$TMP_TEST/joined"

# The fake speaks the two subcommands ensure_tailscale uses: `ip -4` answers
# only after `up` ran, mirroring a box that isn't on a tailnet yet.
cat >"$TMP_TEST/tailscale" <<EOF
#!/bin/sh
case "\$1" in
ip) [ -f "$MARKER" ] || exit 1; printf '100.101.102.103\n' ;;
up) echo "\$@" >"$MARKER" ;;
*) exit 1 ;;
esac
EOF
chmod +x "$TMP_TEST/tailscale"
PATH="$TMP_TEST:$PATH"

# Not on a tailnet and no auth key: must refuse, not hang or guess.
out="$( (KLITE_VPN=tailscale KLITE_TS_AUTHKEY="" KLITE_ADVERTISE="" ensure_tailscale) 2>&1 || true)"
case "$out" in
*KLITE_TS_AUTHKEY*) pass "vpn mode without an auth key refuses" ;;
*) fail "vpn mode without an auth key refuses: got '$out'" ;;
esac

# With an auth key: runs tailscale up, then advertises the derived IP.
KLITE_VPN=tailscale
KLITE_TS_AUTHKEY="tskey-auth-test"
KLITE_ADVERTISE=""
ensure_tailscale >/dev/null
expect_eq "vpn mode derives the advertise address" "$KLITE_ADVERTISE" "100.101.102.103"
case "$(cat "$MARKER")" in
*--auth-key\ tskey-auth-test*) pass "tailscale up received the auth key" ;;
*) fail "tailscale up received the auth key: got '$(cat "$MARKER")'" ;;
esac

# Already on a tailnet (the marker persists): no auth key needed, same answer.
KLITE_TS_AUTHKEY=""
KLITE_ADVERTISE=""
unset ADVERTISE_SOURCE
ensure_tailscale >/dev/null
expect_eq "vpn mode reuses an existing tailnet login" "$KLITE_ADVERTISE" "100.101.102.103"

# An explicit advertise wins over the derived tailnet IP.
KLITE_ADVERTISE="203.0.113.50"
ensure_tailscale >/dev/null
expect_eq "explicit advertise beats the tailnet IP" "$KLITE_ADVERTISE" "203.0.113.50"

# pick_advertise then names tailscale ip -4 as the source.
KLITE_ADVERTISE=""
ensure_tailscale >/dev/null
out="$(pick_advertise 2>&1)"
case "$out" in
*"tailscale ip -4"*) pass "pick_advertise names tailscale as the source" ;;
*) fail "pick_advertise names tailscale as the source: got '$out'" ;;
esac

# --- verdict -------------------------------------------------------------------
if [ "$FAILS" -gt 0 ]; then
    echo "test-join: $FAILS failure(s)" >&2
    exit 1
fi
echo "test-join: all tests passed"
