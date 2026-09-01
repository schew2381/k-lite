package facade

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/schew2381/k-lite/internal/ca"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// SplitEndpoints turns a comma-separated address list into a slice, dropping
// blanks.
func SplitEndpoints(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Creds is what dialing klited requires: a CA to verify it and a bearer
// token it will accept. LoadCreds resolves both the way the CLI does
// (internal/cli/client.go), so one klited state dir serves every client.
type Creds struct {
	transport credentials.TransportCredentials
	token     string
}

// serverFile resolves a file under klited's default state dir. The facade
// reads (never writes) what klited minted there at first boot.
func serverFile(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home, ".klite", "server"}, parts...)...)
}

// LoadCreds builds the transport credentials and admin token. The CA comes
// from caPath, KLITE_CA, or klited's state dir, and insecure trades
// verification away explicitly (klited has no plaintext listener, ADR 0013).
// The token comes from the token argument, KLITE_TOKEN, or the state dir. An
// empty token is allowed: the server answers those calls with its own
// explanation.
func LoadCreds(caPath, token string, insecureSkip bool) (*Creds, error) {
	c := &Creds{}
	if insecureSkip {
		// #nosec G402 -- the --insecure escape hatch trades verification away explicitly
		c.transport = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	} else {
		path := caPath
		if path == "" {
			path = os.Getenv("KLITE_CA")
		}
		if path == "" {
			path = serverFile("tls", "ca.crt")
		}
		caPEM, err := os.ReadFile(path) // #nosec G703 -- the CA path is the operator's own input
		if err != nil {
			return nil, fmt.Errorf("cluster CA unreadable at %s (set --ca or KLITE_CA, or pass --insecure to skip verification): %w", path, err)
		}
		pool, err := ca.PoolFromPEM(caPEM)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		c.transport = credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	}

	c.token = strings.TrimSpace(token)
	if c.token == "" {
		c.token = strings.TrimSpace(os.Getenv("KLITE_TOKEN"))
	}
	if c.token == "" {
		if path := serverFile("token"); path != "" {
			if b, err := os.ReadFile(path); err == nil {
				c.token = strings.TrimSpace(string(b))
			}
		}
	}
	if c.token == "" {
		fmt.Fprintln(os.Stderr, "warning: no admin token found (set --token, KLITE_TOKEN, or let klited mint ~/.klite/server/token)")
	}
	return c, nil
}

// bearerCreds attaches the admin token to every call. Transport security is
// not required here so --insecure keeps working; the wire is TLS either way.
type bearerCreds string

func (b bearerCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (b bearerCreds) RequireTransportSecurity() bool { return false }

func (c *Creds) dialOptions() []grpc.DialOption {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(c.transport)}
	if c.token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearerCreds(c.token)))
	}
	return opts
}

// endpointAddresses builds the resolver addresses for the manual resolver.
// ServerName pins TLS verification to each endpoint's own host; without it
// the handshake checks the placeholder authority from the target URI and
// loops forever against klited's real certificate.
func endpointAddresses(endpoints []string) []resolver.Address {
	addrs := make([]resolver.Address, 0, len(endpoints))
	for _, e := range endpoints {
		addrs = append(addrs, resolver.Address{Addr: e, ServerName: hostOf(e)})
	}
	return addrs
}

// hostOf strips the port for TLS server-name checks, keeping the input when
// it has none.
func hostOf(ep string) string {
	if h, _, err := net.SplitHostPort(ep); err == nil && h != "" {
		return h
	}
	return ep
}

// Dial opens one lazy connection that round-robins over every endpoint, the
// same shape the CLI uses (internal/cli/client.go). WaitForReady queues
// one-shot calls until a backend connects or the request deadline hits.
func Dial(endpoints []string, creds *Creds) (*grpc.ClientConn, klitev1.ClusterServiceClient, error) {
	rb := manual.NewBuilderWithScheme("klite-facade")
	rb.InitialState(resolver.State{Addresses: endpointAddresses(endpoints)})

	opts := append(creds.dialOptions(),
		grpc.WithResolvers(rb),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	)
	conn, err := grpc.NewClient("klite-facade:///control-plane", opts...)
	if err != nil {
		return nil, nil, err
	}
	return conn, klitev1.NewClusterServiceClient(conn), nil
}

// dialOneWith targets a single endpoint for the log walk. No WaitForReady: a
// dead endpoint should fail fast so the walk moves on (see internal/cli/logs.go).
func dialOneWith(creds *Creds) func(addr string) (io.Closer, klitev1.ClusterServiceClient, error) {
	return func(addr string) (io.Closer, klitev1.ClusterServiceClient, error) {
		if creds == nil {
			return nil, nil, fmt.Errorf("no credentials configured")
		}
		conn, err := grpc.NewClient(addr, creds.dialOptions()...)
		if err != nil {
			return nil, nil, err
		}
		return conn, klitev1.NewClusterServiceClient(conn), nil
	}
}
