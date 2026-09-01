GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

BIN := bin
MODULE := github.com/schew2381/k-lite

.PHONY: build proto test lint ui net-image etcd-up etcd-down spike-net spike-envoy clean-docker bootstrap demo

build:
	go build -o $(BIN)/klited ./cmd/klited
	go build -o $(BIN)/klite ./cmd/klite
	go build -o $(BIN)/klite-agent ./cmd/klite-agent

net-image:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BIN)/klite-net-linux-arm64 ./cmd/klite-net
	docker build -f build/klite-net.Dockerfile -t klite-net:dev .

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

spike-net:
	hack/spike-net.sh

spike-envoy:
	hack/spike-envoy/run.sh

clean-docker:
	docker ps -aq --filter label=io.klite.role | xargs docker rm -f 2>/dev/null || true
	docker ps -aq --filter label=io.klite.node | xargs docker rm -f 2>/dev/null || true
	docker network rm klite0 2>/dev/null || true
