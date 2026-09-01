package facade

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

// fakeClient is a canned ClusterServiceClient: List answers from a fixture
// map, PolicyCheck reports Unimplemented (the pre-M6 state), and everything
// else is unreachable in these tests.
type fakeClient struct {
	lists map[string][]*klitev1.Object
}

func (f *fakeClient) List(_ context.Context, req *klitev1.ListRequest, _ ...grpc.CallOption) (*klitev1.ListResponse, error) {
	kind, err := object.Canonical(req.GetKind())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &klitev1.ListResponse{Objects: f.lists[kind]}, nil
}

func (f *fakeClient) PolicyCheck(context.Context, *klitev1.PolicyCheckRequest, ...grpc.CallOption) (*klitev1.PolicyCheckResponse, error) {
	return nil, status.Error(codes.Unimplemented, "PolicyCheck lands in M6")
}

func (f *fakeClient) Apply(context.Context, *klitev1.ApplyRequest, ...grpc.CallOption) (*klitev1.ApplyResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) Delete(context.Context, *klitev1.DeleteRequest, ...grpc.CallOption) (*klitev1.DeleteResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) Scale(context.Context, *klitev1.ScaleRequest, ...grpc.CallOption) (*klitev1.ScaleResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) NodeToken(context.Context, *klitev1.NodeTokenRequest, ...grpc.CallOption) (*klitev1.NodeTokenResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) Uncordon(context.Context, *klitev1.UncordonRequest, ...grpc.CallOption) (*klitev1.UncordonResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) Watch(context.Context, *klitev1.WatchRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[klitev1.WatchEvent], error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) Drain(context.Context, *klitev1.DrainRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[klitev1.DrainProgress], error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func (f *fakeClient) Logs(context.Context, *klitev1.LogsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[klitev1.LogChunk], error) {
	return nil, status.Error(codes.Unimplemented, "not in this test")
}

func fixtureLists() map[string][]*klitev1.Object {
	wrap := func(objs ...*klitev1.Object) []*klitev1.Object { return objs }
	workload := func(name string, replicas, ready int32, labels map[string]string) *klitev1.Object {
		return &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
			Meta:   &klitev1.Meta{Name: name},
			Spec:   &klitev1.WorkloadSpec{Replicas: replicas, Template: &klitev1.Template{Labels: labels}},
			Status: &klitev1.WorkloadStatus{ReadyInstances: ready, TotalInstances: replicas},
		}}}
	}
	node := func(name string, phase klitev1.NodePhase, cordoned bool) *klitev1.Object {
		return &klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
			Meta:   &klitev1.Meta{Name: name},
			Status: &klitev1.NodeStatus{Phase: phase, Unschedulable: cordoned},
		}}}
	}
	service := func(name string, port, target int32, selector map[string]string) *klitev1.Object {
		return &klitev1.Object{Kind: &klitev1.Object_Service{Service: &klitev1.Service{
			Meta: &klitev1.Meta{Name: name},
			Spec: &klitev1.ServiceSpec{Selector: selector, Port: port, TargetPort: target},
		}}}
	}
	instance := func(name, workload, node string, phase klitev1.InstancePhase, restarts int32, ip string) *klitev1.Object {
		return &klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
			Meta:   &klitev1.Meta{Name: name},
			Spec:   &klitev1.InstanceSpec{Workload: workload, Node: node},
			Status: &klitev1.InstanceStatus{Phase: phase, Restarts: restarts, InstanceIp: ip},
		}}}
	}
	policy := &klitev1.Object{Kind: &klitev1.Object_NetworkPolicy{NetworkPolicy: &klitev1.NetworkPolicy{
		Meta: &klitev1.Meta{Name: "allow-only-a-to-b"},
		Spec: &klitev1.NetworkPolicySpec{
			Action: klitev1.PolicyAction_POLICY_ACTION_ALLOW,
			Rules:  []*klitev1.PolicyRule{{From: "a", To: "b"}},
		},
	}}}

	return map[string][]*klitev1.Object{
		object.KindWorkload: wrap(
			workload("b", 2, 2, map[string]string{"app": "b"}),
			workload("a", 1, 0, map[string]string{"app": "a"}),
		),
		object.KindNode: wrap(
			node("node-2", klitev1.NodePhase_NODE_PHASE_NOT_READY, true),
			node("node-1", klitev1.NodePhase_NODE_PHASE_READY, false),
		),
		object.KindService: wrap(
			service("b", 8080, 80, map[string]string{"app": "b"}),
			service("empty", 9090, 90, nil),
		),
		object.KindNetworkPolicy: wrap(policy),
		object.KindInstance: wrap(
			instance("b-abc12", "b", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY, 0, "10.42.1.2"),
			instance("b-def34", "b", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_RUNNING, 3, "10.42.2.2"),
			instance("b-old99", "b", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_FAILED, 5, ""),
			instance("a-zzz00", "a", "", klitev1.InstancePhase_INSTANCE_PHASE_PENDING, 0, ""),
		),
	}
}

func TestComposeTopology(t *testing.T) {
	topo := ComposeTopology(fixtureLists())

	if got := len(topo.Nodes); got != 2 {
		t.Fatalf("nodes: got %d, want 2", got)
	}
	if topo.Nodes[0].Name != "node-1" || topo.Nodes[1].Name != "node-2" {
		t.Errorf("nodes not sorted by name: %+v", topo.Nodes)
	}
	n1 := topo.Nodes[0]
	if n1.Phase != "Ready" || n1.Unschedulable {
		t.Errorf("node-1: got phase=%q unschedulable=%t, want Ready/false", n1.Phase, n1.Unschedulable)
	}
	if len(n1.Instances) != 2 || n1.Instances[0].Name != "b-abc12" || n1.Instances[1].Name != "b-old99" {
		t.Errorf("node-1 instances wrong: %+v", n1.Instances)
	}
	if n1.Instances[0].Phase != "Ready" || n1.Instances[1].Phase != "Failed" {
		t.Errorf("instance phases wrong: %+v", n1.Instances)
	}
	if topo.Nodes[1].Phase != "NotReady" || !topo.Nodes[1].Unschedulable {
		t.Errorf("node-2: got %+v, want NotReady and cordoned", topo.Nodes[1])
	}

	if len(topo.Unscheduled) != 1 || topo.Unscheduled[0].Name != "a-zzz00" {
		t.Errorf("unscheduled: got %+v, want the pending a instance", topo.Unscheduled)
	}

	if len(topo.Services) != 2 {
		t.Fatalf("services: got %d, want 2", len(topo.Services))
	}
	b := topo.Services[0]
	if b.Name != "b" || b.Port != 8080 || b.TargetPort != 80 {
		t.Errorf("service b ports wrong: %+v", b)
	}
	// Ready and Running instances are routing targets; the Failed one is not.
	want := []string{"b-abc12", "b-def34"}
	if len(b.Endpoints) != 2 || b.Endpoints[0] != want[0] || b.Endpoints[1] != want[1] {
		t.Errorf("service b endpoints: got %v, want %v", b.Endpoints, want)
	}
	if got := topo.Services[1]; got.Name != "empty" || len(got.Endpoints) != 0 {
		t.Errorf("empty-selector service must have no endpoints, got %+v", got)
	}

	if len(topo.Policies) != 1 || topo.Policies[0].Action != "ALLOW" || topo.Policies[0].Rules[0].From != "a" {
		t.Errorf("policies wrong: %+v", topo.Policies)
	}

	if len(topo.Workloads) != 2 || topo.Workloads[0].Name != "a" || topo.Workloads[1].Ready != 2 || topo.Workloads[1].Total != 2 {
		t.Errorf("workloads wrong: %+v", topo.Workloads)
	}
}

func newTestServer() *Server {
	return New(&fakeClient{lists: fixtureLists()}, []string{"127.0.0.1:0"}, "", false, nil)
}

func TestTopologyHandler(t *testing.T) {
	srv := httptest.NewServer(newTestServer().Handler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/topology")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var topo Topology
	if err := json.NewDecoder(resp.Body).Decode(&topo); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(topo.Nodes) != 2 || len(topo.Services) != 2 || len(topo.Policies) != 1 {
		t.Errorf("composed topology wrong over HTTP: %+v", topo)
	}
}

func TestPolicyCheckUnimplemented(t *testing.T) {
	srv := httptest.NewServer(newTestServer().Handler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/policycheck?from=a&to=b")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var body struct {
		Available bool `json:"available"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Available {
		t.Error("available: got true, want false while PolicyCheck is Unimplemented")
	}
}

func TestListHandler(t *testing.T) {
	srv := httptest.NewServer(newTestServer().Handler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/workloads")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items: got %d, want 2", len(body.Items))
	}
	if body.Items[0]["kind"] != "Workload" {
		t.Errorf("kind: got %v, want Workload", body.Items[0]["kind"])
	}

	if resp, err := srv.Client().Get(srv.URL + "/api/gadgets"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("unknown kind: got %d, want 404", resp.StatusCode)
		}
	}
}
