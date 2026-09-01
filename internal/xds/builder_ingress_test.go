package xds_test

import (
	"strings"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/xds"
)

// crossNet is service a spread over two nodes, as node-1 sees it: one local
// endpoint with an ingress allocation, one remote draining endpoint with its
// rider, and one remote endpoint whose allocation hasn't landed yet.
func crossNet() *klitev1.NetDesired {
	return &klitev1.NetDesired{
		Services: []*klitev1.ServiceVIP{
			{Service: "a", Vip: "10.44.64.1", Port: 80, TargetPort: 8080},
		},
		Endpoints: []*klitev1.EndpointGroup{
			{Service: "a", Endpoints: []*klitev1.Endpoint{
				{
					Ip: "10.44.128.10", Port: 8080, Node: "node-1",
					Health:      klitev1.EndpointHealth_ENDPOINT_HEALTH_READY,
					IngressPort: 20001, MachineAddress: "192.168.5.2",
				},
				{
					Ip: "10.44.128.20", Port: 8080, Node: "node-2",
					Health:      klitev1.EndpointHealth_ENDPOINT_HEALTH_DRAINING,
					IngressPort: 20033, MachineAddress: "192.168.5.2",
				},
				{
					Ip: "10.44.128.30", Port: 8080, Node: "node-3",
					Health: klitev1.EndpointHealth_ENDPOINT_HEALTH_READY,
				},
			}},
		},
	}
}

func loadAssignment(t *testing.T, snap *cachev3.Snapshot, cluster string) *endpointv3.ClusterLoadAssignment {
	t.Helper()
	res, ok := snap.GetResources(resourcev3.EndpointType)[cluster]
	if !ok {
		t.Fatalf("load assignment %q not in snapshot", cluster)
	}
	return res.(*endpointv3.ClusterLoadAssignment)
}

func getCluster(t *testing.T, snap *cachev3.Snapshot, name string) *clusterv3.Cluster {
	t.Helper()
	res, ok := snap.GetResources(resourcev3.ClusterType)[name]
	if !ok {
		t.Fatalf("cluster %q not in snapshot", name)
	}
	return res.(*clusterv3.Cluster)
}

// The consuming side of ADR 0024: local endpoints stay raw pod addresses,
// remote ones become machineAddress:ingressPort tagged for the mTLS
// transport match, remote ones without a rider vanish (the flat-bridge path
// is dead, not a fallback), and DRAINING survives the rewrite so draining
// stays a source-side decision across the hop.
func TestBuildSnapshotRemoteEndpointsDialIngress(t *testing.T) {
	t.Parallel()
	snap, err := xds.BuildSnapshot("node-1", "1", crossNet())
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	lbs := loadAssignment(t, snap, "a").GetEndpoints()[0].GetLbEndpoints()
	if len(lbs) != 2 {
		t.Fatalf("endpoints = %d, want 2 (the riderless remote endpoint must be dropped)", len(lbs))
	}

	local := lbs[0]
	addr := local.GetEndpoint().GetAddress().GetSocketAddress()
	if addr.GetAddress() != "10.44.128.10" || addr.GetPortValue() != 8080 {
		t.Fatalf("local endpoint = %s:%d, want the raw pod address", addr.GetAddress(), addr.GetPortValue())
	}
	if local.GetMetadata() != nil {
		t.Fatal("local endpoint must carry no transport match metadata")
	}

	remote := lbs[1]
	addr = remote.GetEndpoint().GetAddress().GetSocketAddress()
	if addr.GetAddress() != "192.168.5.2" || addr.GetPortValue() != 20033 {
		t.Fatalf("remote endpoint = %s:%d, want machineAddress:ingressPort", addr.GetAddress(), addr.GetPortValue())
	}
	if remote.GetHealthStatus() != corev3.HealthStatus_DRAINING {
		t.Fatalf("remote health = %v, DRAINING must survive the ingress rewrite", remote.GetHealthStatus())
	}
	md := remote.GetMetadata().GetFilterMetadata()["envoy.transport_socket_match"]
	if md == nil || md.GetFields()["klite"].GetStringValue() != "ingress-mtls" {
		t.Fatalf("remote endpoint metadata = %v, want the ingress-mtls transport tag", remote.GetMetadata())
	}
}

// Service clusters stay stable while endpoints move: the transport match
// list is constant (mTLS for tagged endpoints, an explicit raw fallback) so
// endpoint churn never causes CDS churn, which would drain live connections.
func TestBuildSnapshotClusterTransportMatches(t *testing.T) {
	t.Parallel()
	snap, err := xds.BuildSnapshot("node-1", "1", crossNet())
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	matches := getCluster(t, snap, "a").GetTransportSocketMatches()
	if len(matches) != 2 {
		t.Fatalf("transport matches = %d, want mtls then raw fallback", len(matches))
	}
	mtls := matches[0]
	if mtls.GetMatch().GetFields()["klite"].GetStringValue() != "ingress-mtls" {
		t.Fatalf("first match = %v, want the ingress-mtls tag", mtls.GetMatch())
	}
	up := new(tlsv3.UpstreamTlsContext)
	if err := mtls.GetTransportSocket().GetTypedConfig().UnmarshalTo(up); err != nil {
		t.Fatalf("decode upstream tls: %v", err)
	}
	common := up.GetCommonTlsContext()
	if got := common.GetTlsCertificates()[0].GetCertificateChain().GetFilename(); got != "/etc/klite/tls/node.crt" {
		t.Fatalf("client cert = %q, want the mounted node identity", got)
	}
	if got := common.GetValidationContext().GetTrustedCa().GetFilename(); got != "/etc/klite/tls/ca.crt" {
		t.Fatalf("trusted ca = %q, want the cluster CA", got)
	}
	if common.GetTlsParams().GetTlsMinimumProtocolVersion() != tlsv3.TlsParameters_TLSv1_3 {
		t.Fatal("upstream must pin TLS 1.3")
	}
	if raw := matches[1]; len(raw.GetMatch().GetFields()) != 0 ||
		raw.GetTransportSocket().GetName() != "envoy.transport_sockets.raw_buffer" {
		t.Fatalf("second match = %v/%s, want an empty-match raw fallback", raw.GetMatch(), raw.GetTransportSocket().GetName())
	}
}

// The destination side of ADR 0024: each locally hosted endpoint with an
// allocation gets a 0.0.0.0:<port> listener that terminates mTLS with the
// node identity, demands a CA-chained client cert, and proxies straight to
// the pod through a same-named one-endpoint static cluster.
func TestBuildSnapshotIngressListeners(t *testing.T) {
	t.Parallel()
	snap, err := xds.BuildSnapshot("node-1", "1", crossNet())
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	lst := getListener(t, snap, "ingress/a/20001")
	if lst.GetFreebind().GetValue() {
		t.Fatal("ingress listeners bind 0.0.0.0 and must not carry freebind")
	}
	addr := lst.GetAddress().GetSocketAddress()
	if addr.GetAddress() != "0.0.0.0" || addr.GetPortValue() != 20001 {
		t.Fatalf("ingress bound to %s:%d, want 0.0.0.0:20001", addr.GetAddress(), addr.GetPortValue())
	}

	chain := lst.GetFilterChains()[0]
	down := new(tlsv3.DownstreamTlsContext)
	if err := chain.GetTransportSocket().GetTypedConfig().UnmarshalTo(down); err != nil {
		t.Fatalf("decode downstream tls: %v", err)
	}
	if !down.GetRequireClientCertificate().GetValue() {
		t.Fatal("ingress must require a client certificate")
	}
	if got := down.GetCommonTlsContext().GetTlsCertificates()[0].GetPrivateKey().GetFilename(); got != "/etc/klite/tls/node.key" {
		t.Fatalf("server key = %q, want the mounted node identity", got)
	}

	tcp := new(tcpproxyv3.TcpProxy)
	if err := chain.GetFilters()[0].GetTypedConfig().UnmarshalTo(tcp); err != nil {
		t.Fatalf("decode tcp_proxy: %v", err)
	}
	if tcp.GetCluster() != "ingress/a/20001" {
		t.Fatalf("tcp_proxy cluster = %q, want the paired static cluster", tcp.GetCluster())
	}
	if tcp.GetIdleTimeout().AsDuration() != 0 {
		t.Fatal("idle_timeout zero is deliberate on every tcp_proxy")
	}

	cl := getCluster(t, snap, "ingress/a/20001")
	if cl.GetType() != clusterv3.Cluster_STATIC {
		t.Fatalf("ingress cluster type = %v, want STATIC", cl.GetType())
	}
	pod := cl.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	if pod.GetAddress() != "10.44.128.10" || pod.GetPortValue() != 8080 {
		t.Fatalf("ingress target = %s:%d, want the local pod", pod.GetAddress(), pod.GetPortValue())
	}

	// Remote endpoints never spawn listeners here.
	if _, ok := snap.GetResources(resourcev3.ListenerType)["ingress/a/20033"]; ok {
		t.Fatal("a remote endpoint's ingress listener leaked into this node's snapshot")
	}
}

func TestBuildSnapshotRejectsIngressGarbage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(net *klitev1.NetDesired)
		wantErr string
	}{
		{
			name: "duplicate local ingress port",
			mutate: func(net *klitev1.NetDesired) {
				eps := net.Endpoints[0].Endpoints
				eps[1].Node = "node-1"
				eps[1].IngressPort = 20001
			},
			wantErr: "claimed by two local endpoints",
		},
		{
			name: "hostname machine address",
			mutate: func(net *klitev1.NetDesired) {
				net.Endpoints[0].Endpoints[1].MachineAddress = "host.docker.internal"
			},
			wantErr: "machine address",
		},
		{
			name: "ingress port out of range",
			mutate: func(net *klitev1.NetDesired) {
				net.Endpoints[0].Endpoints[0].IngressPort = 70000
			},
			wantErr: "outside 0-65535",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			net := crossNet()
			tt.mutate(net)
			_, err := xds.BuildSnapshot("node-1", "1", net)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
