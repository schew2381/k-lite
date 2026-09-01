// klite-agent embodies one node: it dials klited, watches the node's desired
// state, and drives Docker to match (ADR 0003, 0004).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/schew2381/k-lite/internal/agent"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

func main() {
	node := flag.String("node", "", "node name this agent embodies (required)")
	server := flag.String("server", "127.0.0.1:7443", "klited gRPC address")
	token := flag.String("cluster-token", "dev-token", "shared secret presented at Register")
	dockerHost := flag.String("docker-host", "", "Docker daemon address, overriding DOCKER_HOST and socket autodetection")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if *node == "" {
		fmt.Fprintln(os.Stderr, "--node is required")
		os.Exit(2)
	}
	if err := run(*node, *server, *token, *dockerHost); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("klite-agent exited", "err", err)
		os.Exit(1)
	}
}

func run(node, server, token, dockerHost string) error {
	// SIGTERM just ends the process. Containers stay up for the next agent
	// run to adopt (see agent.Run).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := runtime.NewDocker(dockerHost)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(server,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Client pings stay above the server's 10s enforcement floor
		// (research/grpc-go.md) and reap dead connections under idle streams.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("grpc client: %w", err)
	}
	defer conn.Close()

	slog.Info("klite-agent starting", "node", node, "server", server)
	a := agent.New(&agent.Config{
		Node:       node,
		Token:      token,
		Runtime:    rt,
		Client:     klitev1.NewAgentServiceClient(conn),
		ServerAddr: server,
	})
	return a.Run(ctx)
}
