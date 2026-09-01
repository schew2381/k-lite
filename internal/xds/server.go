package xds

import (
	"context"
	"fmt"
	"log/slog"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryservice "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Cache holds one snapshot per node. cache.IDHash keys snapshots on the
// Envoy node id, which klited sets to the node name in each bootstrap.
type Cache struct {
	cachev3.SnapshotCache
}

// NewCache returns an ADS-mode snapshot cache logging through slog.
func NewCache() *Cache {
	return &Cache{
		SnapshotCache: cachev3.NewSnapshotCache(true, cachev3.IDHash{}, slogAdapter{slog.Default()}),
	}
}

// SetNodeSnapshot builds, validates, and stores node's snapshot. Version
// must change whenever net does — pass the desired-state revision.
func (c *Cache) SetNodeSnapshot(ctx context.Context, node, version string, net *klitev1.NetDesired) error {
	snap, err := BuildSnapshot(node, version, net)
	if err != nil {
		return err
	}
	if err := c.SetSnapshot(ctx, node, snap); err != nil {
		return fmt.Errorf("node %s: set snapshot: %w", node, err)
	}
	return nil
}

// RegisterADS mounts the xDS services on klited's existing gRPC server. The
// context bounds the lifetime of the xDS stream handlers.
func (c *Cache) RegisterADS(ctx context.Context, grpcServer *grpc.Server) {
	srv := serverv3.NewServer(ctx, c.SnapshotCache, adsCallbacks())
	discoveryservice.RegisterAggregatedDiscoveryServiceServer(grpcServer, srv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, srv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, srv)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, srv)
}

// adsCallbacks surfaces NACKs. Neither go-control-plane's server nor its
// cache logs them, so a node whose Envoy rejects a snapshot would otherwise
// sit on stale config with no trace on the control plane.
func adsCallbacks() serverv3.CallbackFuncs {
	return serverv3.CallbackFuncs{
		StreamRequestFunc: func(_ int64, req *discoveryservice.DiscoveryRequest) error {
			if detail := req.GetErrorDetail(); detail != nil {
				slog.Warn("envoy rejected snapshot (NACK)",
					"node", req.GetNode().GetId(),
					"type", req.GetTypeUrl(),
					"version", req.GetVersionInfo(),
					"err", detail.GetMessage())
			}
			return nil
		},
	}
}

// slogAdapter satisfies go-control-plane's log.Logger with slog.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debugf(format string, args ...any) { a.l.Debug(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Infof(format string, args ...any)  { a.l.Info(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Warnf(format string, args ...any)  { a.l.Warn(fmt.Sprintf(format, args...)) }
func (a slogAdapter) Errorf(format string, args ...any) { a.l.Error(fmt.Sprintf(format, args...)) }
