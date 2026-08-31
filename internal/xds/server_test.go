package xds_test

import (
	"testing"

	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/xds"
)

func TestCacheSetNodeSnapshot(t *testing.T) {
	t.Parallel()
	c := xds.NewCache()
	if err := c.SetNodeSnapshot(t.Context(), "node-1", "42", testNet()); err != nil {
		t.Fatalf("SetNodeSnapshot: %v", err)
	}
	snap, err := c.GetSnapshot("node-1")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got := snap.GetVersion(resourcev3.ListenerType); got != "42" {
		t.Errorf("stored version = %q, want 42", got)
	}
	if _, err := c.GetSnapshot("node-2"); err == nil {
		t.Error("GetSnapshot for unknown node succeeded, want error")
	}
}

func TestCacheSetNodeSnapshotRejectsInconsistent(t *testing.T) {
	t.Parallel()
	c := xds.NewCache()
	net := testNet()
	net.Endpoints = append(net.Endpoints, &klitev1.EndpointGroup{
		Service:   "ghost",
		Endpoints: []*klitev1.Endpoint{{Ip: "10.88.0.13", Port: 80}},
	})
	if err := c.SetNodeSnapshot(t.Context(), "node-1", "1", net); err == nil {
		t.Fatal("SetNodeSnapshot accepted an inconsistent snapshot")
	}
	if _, err := c.GetSnapshot("node-1"); err == nil {
		t.Error("rejected snapshot was stored anyway")
	}
}

func TestRegisterADS(t *testing.T) {
	t.Parallel()
	c := xds.NewCache()
	srv := grpc.NewServer()
	t.Cleanup(srv.Stop)
	c.RegisterADS(t.Context(), srv)
	info := srv.GetServiceInfo()
	for _, name := range []string{
		"envoy.service.discovery.v3.AggregatedDiscoveryService",
		"envoy.service.cluster.v3.ClusterDiscoveryService",
		"envoy.service.endpoint.v3.EndpointDiscoveryService",
		"envoy.service.listener.v3.ListenerDiscoveryService",
	} {
		if _, ok := info[name]; !ok {
			t.Errorf("service %s not registered", name)
		}
	}
}
