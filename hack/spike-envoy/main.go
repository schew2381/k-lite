// Command xds-server is the host-side control plane for spike 2 of ADR 0007
// (Envoy as the day-one data plane). It serves CDS, EDS, and LDS over a
// single ADS stream to one Envoy node and swaps between three snapshots:
//
//	1: listener vip-b proxies TCP to cluster b, no RBAC
//	2: same listener with an RBAC network filter denying -client-ip
//	3: RBAC removed again
//
// Write a phase number and newline to stdin to push the next snapshot.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbaccfgv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	rbacnetv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/rbac/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryservice "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const clusterName = "b"

// topology holds the addresses that the snapshots are built from.
type topology struct {
	backendHost string
	backendPort uint32
	clientIP    string
	vip         string
	vipPort     uint32
}

func main() {
	backend := flag.String("backend", "", "backend as host:port; the sole EDS endpoint of cluster b")
	clientIP := flag.String("client-ip", "", "source IP the phase-2 RBAC filter denies")
	listen := flag.String("listen", "0.0.0.0:18000", "listen address for the ADS gRPC server")
	nodeID := flag.String("node", "spike-node", "Envoy node id the snapshots are keyed on")
	vip := flag.String("vip", "10.44.64.7", "VIP the listener binds to via freebind")
	vipPort := flag.Uint("vip-port", 8080, "VIP listener port")
	phase := flag.Int("phase", 1, "initial phase (1-3)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	if *backend == "" || *clientIP == "" {
		slog.Error("-backend and -client-ip are required")
		os.Exit(2)
	}
	host, portStr, err := net.SplitHostPort(*backend)
	if err != nil {
		slog.Error("bad -backend", "value", *backend, "err", err)
		os.Exit(2)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		slog.Error("bad -backend port", "value", portStr, "err", err)
		os.Exit(2)
	}
	topo := topology{
		backendHost: host,
		backendPort: uint32(port),
		clientIP:    *clientIP,
		vip:         *vip,
		vipPort:     uint32(*vipPort),
	}

	snapCache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, slogAdapter{logger})
	setPhase := func(p int) error {
		snap, err := buildSnapshot(p, topo)
		if err != nil {
			return fmt.Errorf("build snapshot: %w", err)
		}
		if err := snap.Consistent(); err != nil {
			return fmt.Errorf("inconsistent snapshot: %w", err)
		}
		// Distinct version per phase; Envoy ignores a snapshot whose version
		// matches the one it already ACKed.
		if err := snapCache.SetSnapshot(context.Background(), *nodeID, snap); err != nil {
			return fmt.Errorf("set snapshot: %w", err)
		}
		slog.Info("snapshot set", "phase", p, "version", strconv.Itoa(p), "rbac", p == 2)
		return nil
	}
	if err := setPhase(*phase); err != nil {
		slog.Error("initial snapshot failed", "err", err)
		os.Exit(1)
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		slog.Error("listen failed", "addr", *listen, "err", err)
		os.Exit(1)
	}
	xds := serverv3.NewServer(context.Background(), snapCache, nil)
	grpcServer := grpc.NewServer()
	discoveryservice.RegisterAggregatedDiscoveryServiceServer(grpcServer, xds)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xds)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xds)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xds)
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("grpc server stopped", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("ads server listening", "addr", *listen, "node", *nodeID)

	// Phase swaps arrive on stdin, one number per line. run.sh keeps the
	// write end of the pipe open for the lifetime of the script, so EOF here
	// doubles as the shutdown signal.
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		p, err := strconv.Atoi(line)
		if err != nil || p < 1 || p > 3 {
			slog.Warn("ignoring stdin line", "line", line)
			continue
		}
		if err := setPhase(p); err != nil {
			slog.Error("snapshot swap failed", "phase", p, "err", err)
			os.Exit(1)
		}
	}
	slog.Info("stdin closed, shutting down")
	grpcServer.Stop()
}

func buildSnapshot(phase int, t topology) (*cachev3.Snapshot, error) {
	lst, err := makeListener(t, phase == 2)
	if err != nil {
		return nil, err
	}
	return cachev3.NewSnapshot(strconv.Itoa(phase), map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  {makeCluster()},
		resourcev3.EndpointType: {makeEndpoints(t)},
		resourcev3.ListenerType: {lst},
	})
}

func makeCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 clusterName,
		ConnectTimeout:       durationpb.New(time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_EDS},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: &corev3.ConfigSource{
				ResourceApiVersion:    corev3.ApiVersion_V3,
				ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
			},
		},
	}
}

func makeEndpoints(t topology) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: socketAddress(t.backendHost, t.backendPort),
					},
				},
			}},
		}},
	}
}

// makeListener builds the vip-b listener: optional RBAC filter, then
// tcp_proxy to cluster b. Freebind (IP_FREEBIND) lets the socket bind before
// the VIP exists on any interface; run.sh adds the address to eth0 only after
// Envoy reports the listener active, so a working bind proves freebind took
// effect.
func makeListener(t topology, withRBAC bool) (*listenerv3.Listener, error) {
	tcpProxy, err := anypb.New(&tcpproxyv3.TcpProxy{
		StatPrefix:       "vip_b",
		ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: clusterName},
	})
	if err != nil {
		return nil, err
	}

	var filters []*listenerv3.Filter
	if withRBAC {
		rbacAny, err := anypb.New(denyClientRBAC(t.clientIP))
		if err != nil {
			return nil, err
		}
		filters = append(filters, &listenerv3.Filter{
			Name:       "envoy.filters.network.rbac",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: rbacAny},
		})
	}
	filters = append(filters, &listenerv3.Filter{
		Name:       "envoy.filters.network.tcp_proxy",
		ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: tcpProxy},
	})

	return &listenerv3.Listener{
		Name:         "vip-b",
		Freebind:     wrapperspb.Bool(true),
		Address:      socketAddress(t.vip, t.vipPort),
		FilterChains: []*listenerv3.FilterChain{{Filters: filters}},
	}, nil
}

// denyClientRBAC denies connections whose direct (socket-level) source IP is
// clientIP and allows everyone else.
func denyClientRBAC(clientIP string) *rbacnetv3.RBAC {
	return &rbacnetv3.RBAC{
		StatPrefix: "vip_b",
		Rules: &rbaccfgv3.RBAC{
			Action: rbaccfgv3.RBAC_DENY,
			Policies: map[string]*rbaccfgv3.Policy{
				"deny-spike-client": {
					Permissions: []*rbaccfgv3.Permission{{
						Rule: &rbaccfgv3.Permission_Any{Any: true},
					}},
					Principals: []*rbaccfgv3.Principal{{
						Identifier: &rbaccfgv3.Principal_DirectRemoteIp{
							DirectRemoteIp: &corev3.CidrRange{
								AddressPrefix: clientIP,
								PrefixLen:     wrapperspb.UInt32(32),
							},
						},
					}},
				},
			},
		},
	}
}

func socketAddress(host string, port uint32) *corev3.Address {
	return &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol:      corev3.SocketAddress_TCP,
				Address:       host,
				PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
			},
		},
	}
}

// slogAdapter satisfies go-control-plane's log.Logger with slog.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debugf(format string, args ...any) { a.l.Debug(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Infof(format string, args ...any)  { a.l.Info(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Warnf(format string, args ...any)  { a.l.Warn(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Errorf(format string, args ...any) { a.l.Error(fmt.Sprintf(format, args...)) }
