GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

BIN := bin
MODULE := github.com/schew2381/k-lite

DIST := dist
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/arm64
RELEASE_BINS := klite klited klite-agent klite-net
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo "shasum -a 256")

# The local daemon's arch for `make net-image` (colima on Apple Silicon is
# arm64). Override with NET_ARCH=amd64 when the daemon differs from the host.
NET_ARCH ?= $(shell go env GOARCH)

.PHONY: build release proto test lint ui net-image etcd-up etcd-down spike-net spike-envoy clean-docker bootstrap demo

build:
	go build -o $(BIN)/klited ./cmd/klited
	go build -o $(BIN)/klite ./cmd/klite
	go build -o $(BIN)/klite-agent ./cmd/klite-agent

# Static cross-builds of every release artifact (ADR 0038): pure Go, CGO off,
# the recipe real-nodes.md verified. The workflow attaches dist/* to the
# GitHub Release on every version tag.
release:
	rm -rf $(DIST)
	mkdir -p $(DIST)
	@set -e; for platform in $(RELEASE_PLATFORMS); do \
		for bin in $(RELEASE_BINS); do \
			echo "  $$bin $$platform"; \
			CGO_ENABLED=0 GOOS=$${platform%/*} GOARCH=$${platform#*/} \
				go build -trimpath -o $(DIST)/$$bin-$${platform%/*}-$${platform#*/} ./cmd/$$bin; \
		done; \
	done
	cd $(DIST) && $(SHA256) klite* > checksums.txt

net-image:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(NET_ARCH) go build -o $(BIN)/klite-net-linux-$(NET_ARCH) ./cmd/klite-net
	docker build -f build/klite-net.Dockerfile --build-arg TARGETARCH=$(NET_ARCH) -t klite-net:dev .

proto:
	go tool buf format -w
	go tool buf generate

proto-lint:
	go tool buf lint
	go tool buf format --diff --exit-code
	go tool buf breaking --against '.git#branch=main'

test:
	go vet ./...
	go test -race -count=1 -shuffle=on ./...

lint:
	golangci-lint run ./...

fmt:
	gofumpt -l -w cmd internal/cli internal/object internal/store internal/leader internal/server internal/scheduler internal/controller internal/runtime internal/agent internal/netd internal/policy internal/xds internal/wire 2>/dev/null || gofumpt -l -w .

precommit:
	prek run --all-files

ui:
	cd frontend && bun install --frozen-lockfile && VITE_KLITE_MODE=http bun run build

# Fresh-machine setup: brew tools, colima, base images. Safe to re-run.
bootstrap:
	hack/bootstrap.sh

# The presentation run: fresh cluster, every headline feature, live board.
demo:
	hack/demo.sh

# make node-1 runs an agent for node-1. Any suffix works.
node-%:
	go run ./cmd/klite-agent --node node-$*

etcd-up:
	hack/etcd-up.sh

etcd-down:
	hack/etcd-up.sh down

down:
	hack/dev-down.sh --all

spike-net:
	hack/spike-net.sh

spike-envoy:
	hack/spike-envoy/run.sh

clean-docker:
	docker ps -aq --filter label=io.klite.role | xargs docker rm -f 2>/dev/null || true
	docker ps -aq --filter label=io.klite.node | xargs docker rm -f 2>/dev/null || true
	docker network rm klite0 2>/dev/null || true
