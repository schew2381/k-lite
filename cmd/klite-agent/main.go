// klite-agent embodies one node: it dials klited, watches the node's desired
// state, and drives Docker to match (ADR 0003, 0004). Since M8 every dial
// carries the node's CA-signed identity, joined via token on first contact
// (ADR 0013).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/schew2381/k-lite/internal/agent"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

func main() {
	node := flag.String("node", "", "node name this agent embodies (required)")
	server := flag.String("server", "127.0.0.1:7443", "klited gRPC address(es), comma-separated; streams fail over between them")
	token := flag.String("token", "", "join credential: a K10 token from `klite node token`, or the bare cluster secret when the CA is readable locally")
	clusterToken := flag.String("cluster-token", "", "deprecated alias for --token")
	stateDir := flag.String("state-dir", "", "root for per-node files, identity included (default ~/.klite/agent)")
	dockerHost := flag.String("docker-host", "", "Docker daemon address, overriding DOCKER_HOST and socket autodetection")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if *node == "" {
		fmt.Fprintln(os.Stderr, "--node is required")
		os.Exit(2)
	}
	if *token == "" {
		*token = *clusterToken
	}
	if err := run(*node, *server, *token, *stateDir, *dockerHost); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("klite-agent exited", "err", err)
		os.Exit(1)
	}
}

func run(node, server, token, stateDir, dockerHost string) error {
	// SIGTERM just ends the process. Containers stay up for the next agent
	// run to adopt (see agent.Run).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	endpoints := splitEndpoints(server)
	if len(endpoints) == 0 {
		return errors.New("--server needs at least one address")
	}
	rt, err := runtime.NewDocker(dockerHost)
	if err != nil {
		return err
	}
	ident, err := agent.EnsureIdentity(ctx, &agent.JoinConfig{
		Node:      node,
		Endpoints: endpoints,
		Token:     token,
		StateDir:  stateDir,
	})
	if err != nil {
		return err
	}
	conn, err := dialControlPlane(endpoints, ident)
	if err != nil {
		return fmt.Errorf("grpc client: %w", err)
	}
	defer conn.Close()

	slog.Info("klite-agent starting", "node", node, "servers", strings.Join(endpoints, ","), "identity", ident.Dir)
	a := agent.New(&agent.Config{
		Node:        node,
		Token:       token,
		Runtime:     rt,
		Client:      klitev1.NewAgentServiceClient(conn),
		ServerAddrs: endpoints,
		StateDir:    stateDir,
		TLSDir:      ident.Dir,
		// The command plane pins one endpoint per stream life: output
		// pushes only route on the klited that sent the command.
		CommandDial: func(endpoint string) (*grpc.ClientConn, error) {
			return grpc.NewClient(endpoint,
				grpc.WithTransportCredentials(credentials.NewTLS(ident.TLS)),
				grpc.WithKeepaliveParams(keepalive.ClientParameters{
					Time:                20 * time.Second,
					Timeout:             10 * time.Second,
					PermitWithoutStream: true,
				}),
			)
		},
	})
	return a.Run(ctx)
}

// dialControlPlane opens one lazy connection that round-robins over every
// klited, so streams broken by a dead replica resume on a survivor
// (research/grpc-go.md, the CLI's failover shape).
func dialControlPlane(endpoints []string, ident *agent.Identity) (*grpc.ClientConn, error) {
	addrs := make([]resolver.Address, 0, len(endpoints))
	for _, e := range endpoints {
		// ServerName pins TLS verification to the endpoint's own host;
		// without it the handshake would check the placeholder authority.
		addrs = append(addrs, resolver.Address{Addr: e, ServerName: hostOf(e)})
	}
	rb := manual.NewBuilderWithScheme("klite-agent")
	rb.InitialState(resolver.State{Addresses: addrs})
	return grpc.NewClient("klite-agent:///control-plane",
		grpc.WithResolvers(rb),
		grpc.WithTransportCredentials(credentials.NewTLS(ident.TLS)),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		// Client pings stay above the server's 10s enforcement floor
		// (research/grpc-go.md) and reap dead connections under idle streams.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

func splitEndpoints(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hostOf(ep string) string {
	if h, _, err := net.SplitHostPort(ep); err == nil && h != "" {
		return h
	}
	return ep
}
