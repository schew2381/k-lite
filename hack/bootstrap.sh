#!/usr/bin/env bash
# Fresh-machine setup for k-lite dev: brew tools, colima, base images, go sanity.
# Safe to re-run, and it prints what it skipped.
set -uo pipefail

SKIPPED=()
skip() { SKIPPED+=("$1"); }
say() { echo "==> $*"; }
fail=0

if ! command -v brew >/dev/null 2>&1; then
  echo "bootstrap: Homebrew not found. Install it first: https://brew.sh" >&2
  exit 1
fi

# --- brew packages ---
for pkg in go colima docker golangci-lint gofumpt prek bun; do
  if command -v "$pkg" >/dev/null 2>&1 || brew list --formula "$pkg" >/dev/null 2>&1; then
    skip "brew install $pkg (already installed)"
  else
    say "brew install $pkg"
    brew install "$pkg" || { echo "bootstrap: brew install $pkg failed" >&2; fail=1; }
  fi
done

# --- colima ---
if colima status >/dev/null 2>&1; then
  skip "colima start (already running)"
else
  say "colima start (4 CPU / 8 GiB, vz)"
  colima start --cpu 4 --memory 8 --vm-type vz || { echo "bootstrap: colima start failed" >&2; fail=1; }
fi

if ! docker info >/dev/null 2>&1; then
  echo "bootstrap: docker daemon not reachable (is colima up?)" >&2
  fail=1
fi

# --- base images ---
for img in alpine:3.20 traefik/whoami:v1.10 quay.io/coreos/etcd:v3.5.16 envoyproxy/envoy:v1.31.5; do
  if docker image inspect "$img" >/dev/null 2>&1; then
    skip "docker pull $img (already present)"
  else
    say "docker pull $img"
    docker pull "$img" || { echo "bootstrap: pull $img failed" >&2; fail=1; }
  fi
done

# --- go sanity ---
if command -v go >/dev/null 2>&1; then
  say "go: $(go version)"
  echo "    GOPATH=$(go env GOPATH)  GOOS=$(go env GOOS)  GOARCH=$(go env GOARCH)"
  want="$(awk '$1=="go" {print $2; exit}' "$(dirname "$0")/../go.mod" 2>/dev/null)"
  [[ -n "$want" ]] && echo "    go.mod wants go >= $want (toolchain auto-downloads if needed)"
else
  echo "bootstrap: go not on PATH after install. Open a new shell or add \$(brew --prefix)/bin" >&2
  fail=1
fi

echo
if [[ "${#SKIPPED[@]}" -gt 0 ]]; then
  echo "skipped (already done):"
  for s in "${SKIPPED[@]}"; do echo "  - $s"; done
fi
if [[ "$fail" == 1 ]]; then
  echo "bootstrap: finished with errors (see above)" >&2
  exit 1
fi
echo "bootstrap: ready. next: make demo (the full show) or hack/dev-up.sh (just a playground)"
