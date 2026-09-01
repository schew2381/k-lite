package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/schew2381/k-lite/internal/ca"
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

// nodeCertCtx builds a peer context carrying a verified client cert for node,
// the shape credentials.NewTLS leaves behind after VerifyClientCertIfGiven.
func nodeCertCtx(t *testing.T, node string) context.Context {
	t.Helper()
	authority, err := ca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	csrPEM, _, err := ca.NewNodeCSR(node)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := authority.SignNodeCSR(csrPEM, node)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{cert, authority.Cert}},
		}},
	})
}

func bearerCtx(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

func TestAuthorizeMatrix(t *testing.T) {
	t.Parallel()
	a := NewAuth("secret")
	nodeCtx := nodeCertCtx(t, "node-1")

	tests := []struct {
		name   string
		ctx    context.Context
		method string
		want   codes.Code
	}{
		{"register open to anonymous", context.Background(), "/klite.v1.AgentService/Register", codes.OK},
		{"agent rpc needs a cert", context.Background(), "/klite.v1.AgentService/ReportStatus", codes.Unauthenticated},
		{"agent rpc rejects admin", bearerCtx("secret"), "/klite.v1.AgentService/ReportStatus", codes.PermissionDenied},
		{"agent rpc accepts node cert", nodeCtx, "/klite.v1.AgentService/ReportStatus", codes.OK},
		{"xds needs a cert", context.Background(), "/envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources", codes.Unauthenticated},
		{"xds accepts node cert", nodeCtx, "/envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources", codes.OK},
		{"cluster rpc needs the token", context.Background(), "/klite.v1.ClusterService/List", codes.Unauthenticated},
		{"cluster rpc rejects bad token", bearerCtx("wrong"), "/klite.v1.ClusterService/List", codes.Unauthenticated},
		{"cluster rpc rejects node cert", nodeCtx, "/klite.v1.ClusterService/List", codes.PermissionDenied},
		{"cluster rpc accepts the token", bearerCtx("secret"), "/klite.v1.ClusterService/List", codes.OK},
		{"unknown service denied even with token", bearerCtx("secret"), "/grpc.health.v1.Health/Check", codes.PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := a.authorize(tt.ctx, tt.method)
			if got := status.Code(err); got != tt.want {
				t.Fatalf("authorize(%s) = %v (%v), want %v", tt.method, got, err, tt.want)
			}
		})
	}
}

// recvQueue fakes the wire side of an xDS stream: RecvMsg copies the next
// queued DiscoveryRequest into m.
type recvQueue struct {
	grpc.ServerStream
	ctx   context.Context
	queue []*discoveryv3.DiscoveryRequest
}

func (r *recvQueue) Context() context.Context { return r.ctx }

func (r *recvQueue) RecvMsg(m any) error {
	if len(r.queue) == 0 {
		return io.EOF
	}
	next := r.queue[0]
	r.queue = r.queue[1:]
	proto.Merge(m.(proto.Message), next)
	return nil
}

// ADS streams are bound to the certificate's node (ADR 0028's residual):
// the first request must name it, node-less follow-ups ride the binding,
// and naming any other node — first or later — is denied.
func TestXDSStreamBoundToCertNode(t *testing.T) {
	t.Parallel()
	req := func(id string) *discoveryv3.DiscoveryRequest {
		if id == "" {
			return &discoveryv3.DiscoveryRequest{VersionInfo: "1"}
		}
		return &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: id}}
	}
	tests := []struct {
		name  string
		queue []*discoveryv3.DiscoveryRequest
		want  []codes.Code
	}{
		{"own node then node-less acks", []*discoveryv3.DiscoveryRequest{req("node-1"), req(""), req("node-1")}, []codes.Code{codes.OK, codes.OK, codes.OK}},
		{"foreign node on first request", []*discoveryv3.DiscoveryRequest{req("node-2")}, []codes.Code{codes.PermissionDenied}},
		{"rename mid-stream", []*discoveryv3.DiscoveryRequest{req("node-1"), req("node-2")}, []codes.Code{codes.OK, codes.PermissionDenied}},
		{"anonymous first request", []*discoveryv3.DiscoveryRequest{req("")}, []codes.Code{codes.PermissionDenied}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &xdsBoundStream{
				authedStream: authedStream{ServerStream: &recvQueue{queue: tt.queue}},
				node:         "node-1",
			}
			for i, want := range tt.want {
				err := s.RecvMsg(&discoveryv3.DiscoveryRequest{})
				if got := status.Code(err); got != want {
					t.Fatalf("recv %d = %v (%v), want %v", i, got, err, want)
				}
			}
		})
	}
}

// The interceptor wraps only xDS streams with the binding; agent streams
// keep their request-level node checks.
func TestStreamInterceptorWrapsXDS(t *testing.T) {
	t.Parallel()
	a := NewAuth("secret")
	interceptor := a.Stream()
	nodeCtx := nodeCertCtx(t, "node-1")
	var got grpc.ServerStream
	capture := func(_ any, ss grpc.ServerStream) error { got = ss; return nil }

	err := interceptor(nil, &recvQueue{ctx: nodeCtx}, &grpc.StreamServerInfo{
		FullMethod: "/envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources",
	}, capture)
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := got.(*xdsBoundStream)
	if !ok {
		t.Fatalf("xds stream wrapped as %T, want *xdsBoundStream", got)
	}
	if bound.node != "node-1" {
		t.Fatalf("bound node = %q, want the certificate's", bound.node)
	}

	err = interceptor(nil, &recvQueue{ctx: nodeCtx}, &grpc.StreamServerInfo{
		FullMethod: "/klite.v1.AgentService/WatchDesired",
	}, capture)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*xdsBoundStream); ok {
		t.Fatal("agent stream must not carry the xds binding")
	}
}

func TestEmptyAdminTokenDisablesAdmin(t *testing.T) {
	t.Parallel()
	a := NewAuth("")
	if _, err := a.authorize(bearerCtx(""), "/klite.v1.ClusterService/List"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("empty bearer against empty configured token must not authenticate, got %v", err)
	}
}

func TestRequireNodeMatch(t *testing.T) {
	t.Parallel()
	a := NewAuth("secret")
	ctx, err := a.authorize(nodeCertCtx(t, "node-1"), "/klite.v1.AgentService/ReportStatus")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireNodeMatch(ctx, "node-1"); err != nil {
		t.Fatalf("own node rejected: %v", err)
	}
	if err := requireNodeMatch(ctx, "node-2"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign node allowed: %v", err)
	}
	// Without the interceptor (unit-test servers) there is no principal and
	// the check stays out of the way.
	if err := requireNodeMatch(context.Background(), "node-2"); err != nil {
		t.Fatalf("principal-free context rejected: %v", err)
	}
}

func TestRegisterSignsCSRAndAcceptsCertOnly(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	seedNode(t, st, "node-1", klitev1.NodePhase_NODE_PHASE_UNSPECIFIED)
	authority, err := ca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuth("secret")
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "join-me", CA: authority, ClusterID: "abc123"})

	csrPEM, _, err := ca.NewNodeCSR("node-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := auth.authorize(context.Background(), "/klite.v1.AgentService/Register")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Register(ctx, &klitev1.RegisterRequest{Node: "node-1", ClusterToken: "join-me", CsrPem: csrPEM})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetCertPem()) == 0 || len(resp.GetCaPem()) == 0 {
		t.Fatal("join with CSR must return the node cert and the CA")
	}
	if resp.GetNet().GetClusterId() != "abc123" {
		t.Fatalf("cluster id not handed out, got %q", resp.GetNet().GetClusterId())
	}

	// A certified node re-registers with no token at all.
	certCtx, err := auth.authorize(nodeCertCtx(t, "node-1"), "/klite.v1.AgentService/Register")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Register(certCtx, &klitev1.RegisterRequest{Node: "node-1"}); err != nil {
		t.Fatalf("cert-only re-register: %v", err)
	}
	// But a certificate for one node buys nothing for another name.
	if _, err := a.Register(certCtx, &klitev1.RegisterRequest{Node: "node-2"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("cert for node-1 registering node-2 without token: got %v", err)
	}
}

func TestNetBootstrapHonorsBases(t *testing.T) {
	t.Parallel()
	a := NewAgent(&AgentConfig{NetAdminPortBase: 21000, EnvoyAdminPortBase: 21500, InfraIPBase: 60})
	nb := a.netBootstrap(3)
	if nb.GetKliteNetIp() != "10.44.0.63" {
		t.Fatalf("infra ip base ignored: %s", nb.GetKliteNetIp())
	}
	if nb.GetNetAdminPortBase() != 21000 || nb.GetEnvoyAdminPortBase() != 21500 {
		t.Fatalf("port bases not forwarded: %v", nb)
	}
	if def := NewAgent(&AgentConfig{}).netBootstrap(3); def.GetKliteNetIp() != "10.44.0.13" {
		t.Fatalf("default infra ip broken: %s", def.GetKliteNetIp())
	}
}

func TestUncordon(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	s := NewCluster(st, NewCommandHub(), nil)

	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	if err := s.Drain(&klitev1.DrainRequest{Node: "node-2"}, newDrainStream(ctx)); err != nil {
		t.Fatal(err)
	}
	obj, _, err := st.Get(ctx, "node", "node-2")
	if err != nil {
		t.Fatal(err)
	}
	if !obj.GetNode().GetStatus().GetUnschedulable() {
		t.Fatal("drain should cordon the node first")
	}

	if _, err := s.Uncordon(ctx, &klitev1.UncordonRequest{Node: "node-2"}); err != nil {
		t.Fatal(err)
	}
	obj, _, err = st.Get(ctx, "node", "node-2")
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetNode().GetStatus().GetUnschedulable() {
		t.Fatal("uncordon left the node unschedulable")
	}

	// A pending-delete node stays cordoned: the delete choreography owns it.
	if _, err := s.Delete(ctx, &klitev1.DeleteRequest{Kind: "node", Name: "node-2"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Uncordon(ctx, &klitev1.UncordonRequest{Node: "node-2"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("uncordon of a pending-delete node: got %v, want FailedPrecondition", err)
	}
}

func TestNodeTokenMintsPinnedToken(t *testing.T) {
	t.Parallel()
	authority, err := ca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewCluster(storetest.New(), NewCommandHub(), &TokenConfig{CAPEM: authority.CertPEM, ClusterSecret: "join-me"})
	resp, err := s.NodeToken(context.Background(), &klitev1.NodeTokenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ca.ParseToken(resp.GetToken())
	if err != nil {
		t.Fatalf("minted token does not parse: %v", err)
	}
	if tok.Secret != "join-me" {
		t.Fatalf("token secret = %q", tok.Secret)
	}
	if err := ca.VerifyCAHash(authority.CertPEM, tok.CAHash); err != nil {
		t.Fatalf("token pin does not match the CA: %v", err)
	}

	none := NewCluster(storetest.New(), NewCommandHub(), nil)
	if _, err := none.NodeToken(context.Background(), &klitev1.NodeTokenRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("CA-less server should refuse to mint, got %v", err)
	}
}
