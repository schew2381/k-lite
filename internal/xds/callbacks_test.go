package xds

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryservice "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The NACK logger must treat ACKs and NACKs alike as non-fatal: returning an
// error from OnStreamRequest would kill the Envoy's ADS stream.
func TestADSCallbacksNeverFailStream(t *testing.T) {
	t.Parallel()
	cb := adsCallbacks()
	ack := &discoveryservice.DiscoveryRequest{VersionInfo: "3"}
	nack := &discoveryservice.DiscoveryRequest{
		VersionInfo: "3",
		Node:        &corev3.Node{Id: "node-1"},
		ErrorDetail: status.New(codes.InvalidArgument, "bad listener").Proto(),
	}
	for name, req := range map[string]*discoveryservice.DiscoveryRequest{"ack": ack, "nack": nack} {
		if err := cb.StreamRequestFunc(1, req); err != nil {
			t.Errorf("%s: OnStreamRequest returned %v, want nil", name, err)
		}
	}
}
