package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

const defaultServer = "127.0.0.1:7443"

type fileConfig struct {
	Endpoints []string `json:"endpoints"`
}

// endpoints resolves the server list: --server flag, then KLITE_SERVER, then ~/.klite/config, then the default.
func endpoints(flagValue string) []string {
	if flagValue != "" {
		return splitList(flagValue)
	}
	if env := os.Getenv("KLITE_SERVER"); env != "" {
		return splitList(env)
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".klite", "config")
		if b, err := os.ReadFile(path); err == nil {
			var c fileConfig
			if err := yaml.Unmarshal(b, &c); err != nil {
				fmt.Fprintf(os.Stderr, "warning: ignoring %s: %v\n", path, err)
			} else if len(c.Endpoints) > 0 {
				return c.Endpoints
			}
		}
	}
	return []string{defaultServer}
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// dial opens one lazy connection that round-robins over every endpoint, so any
// live klited answers (research/grpc-go.md). WaitForReady queues one-shot calls
// until a backend connects or the command deadline hits.
func dial(server string) (*grpc.ClientConn, klitev1.ClusterServiceClient, error) {
	eps := endpoints(server)
	addrs := make([]resolver.Address, 0, len(eps))
	for _, e := range eps {
		addrs = append(addrs, resolver.Address{Addr: e})
	}
	rb := manual.NewBuilderWithScheme("klite")
	rb.InitialState(resolver.State{Addresses: addrs})
	resolver.Register(rb)

	conn, err := grpc.NewClient("klite:///control-plane",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	)
	if err != nil {
		return nil, nil, err
	}
	return conn, klitev1.NewClusterServiceClient(conn), nil
}

// dialOne targets a single endpoint, unlike dial's round-robin pool. Log
// streams need it because only the klited holding the target agent's command
// stream can serve them, so the caller walks endpoints itself. No
// WaitForReady here: a dead endpoint should fail fast so the walk moves on.
func dialOne(addr string) (*grpc.ClientConn, klitev1.ClusterServiceClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return conn, klitev1.NewClusterServiceClient(conn), nil
}

// rpcErr unwraps a gRPC status so users see the message, not the wire framing.
func rpcErr(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := status.FromError(err); ok {
		return fmt.Errorf("%s", s.Message())
	}
	return err
}
