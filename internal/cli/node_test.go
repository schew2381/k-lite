package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

const testJoinToken = "K10abc::node:s3cret" // #nosec G101 -- made-up test fixture, not a credential

// fakeNodeAddClient stubs the two RPCs node add uses. The embedded interface
// panics on anything else, which is exactly the guard we want.
type fakeNodeAddClient struct {
	klitev1.ClusterServiceClient
	appliedYAML []byte
	applyErr    error
	token       string
	tokenCalls  int
}

func (f *fakeNodeAddClient) Apply(_ context.Context, in *klitev1.ApplyRequest, _ ...grpc.CallOption) (*klitev1.ApplyResponse, error) {
	f.appliedYAML = in.GetYaml()
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &klitev1.ApplyResponse{Results: []*klitev1.ApplyResult{
		{Kind: "Node", Name: "node-4", Action: "created"},
	}}, nil
}

func (f *fakeNodeAddClient) NodeToken(context.Context, *klitev1.NodeTokenRequest, ...grpc.CallOption) (*klitev1.NodeTokenResponse, error) {
	f.tokenCalls++
	return &klitev1.NodeTokenResponse{Token: f.token}, nil
}

func TestNodeAddAppliesAndPrintsJoinBlock(t *testing.T) {
	t.Parallel()
	fake := &fakeNodeAddClient{token: testJoinToken}
	var out strings.Builder
	err := runNodeAdd(context.Background(), fake, &out, &nodeAddOpts{
		name:         "node-4",
		labels:       map[string]string{"zone": "sfo"},
		maxInstances: 8,
		url:          "203.0.113.7:7443",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The manifest must round-trip through the real codec with every flag
	// applied, or the printed join line points at a node that differs from
	// the declared one.
	objs, err := object.Decode(fake.appliedYAML)
	if err != nil {
		t.Fatalf("applied YAML does not decode: %v\n%s", err, fake.appliedYAML)
	}
	if len(objs) != 1 {
		t.Fatalf("decoded %d objects, want 1", len(objs))
	}
	node := objs[0].GetNode()
	if node == nil {
		t.Fatalf("applied object is not a Node: %v", objs[0])
	}
	if got := node.GetMeta().GetName(); got != "node-4" {
		t.Errorf("node name = %q", got)
	}
	if got := node.GetMeta().GetLabels()["zone"]; got != "sfo" {
		t.Errorf("labels = %v, want zone=sfo", node.GetMeta().GetLabels())
	}
	if got := node.GetSpec().GetMaxInstances(); got != 8 {
		t.Errorf("maxInstances = %d, want 8", got)
	}

	if fake.tokenCalls != 1 {
		t.Fatalf("NodeToken called %d times, want 1", fake.tokenCalls)
	}
	got := out.String()
	for _, want := range []string{
		"node/node-4 created",
		"releases/latest/download/join.sh",
		"KLITE_URL=203.0.113.7:7443 KLITE_TOKEN='K10abc::node:s3cret' KLITE_NODE=node-4 sh -",
		"sudo ./klite-agent --node node-4 --server 203.0.113.7:7443",
		"--token 'K10abc::node:s3cret'",
		"--advertise-address",
		"172.17.0.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "loopback") {
		t.Errorf("loopback note printed for a routable URL:\n%s", got)
	}
}

// Bare defaults must produce a minimal manifest (server defaulting owns
// maxInstances) and flag a loopback URL, which the new machine cannot dial.
func TestNodeAddDefaultsAndLoopbackNote(t *testing.T) {
	t.Parallel()
	fake := &fakeNodeAddClient{token: testJoinToken}
	var out strings.Builder
	err := runNodeAdd(context.Background(), fake, &out, &nodeAddOpts{
		name: "node-4",
		url:  "127.0.0.1:7443",
	})
	if err != nil {
		t.Fatal(err)
	}
	y := string(fake.appliedYAML)
	if strings.Contains(y, "labels") || strings.Contains(y, "maxInstances") {
		t.Errorf("bare add must omit optional fields:\n%s", y)
	}
	objs, err := object.Decode(fake.appliedYAML)
	if err != nil || len(objs) != 1 || objs[0].GetNode() == nil {
		t.Fatalf("bare manifest does not decode to a Node: %v\n%s", err, y)
	}
	if !strings.Contains(out.String(), "loopback") {
		t.Errorf("loopback URL must carry the reachability note:\n%s", out.String())
	}
}

func TestNodeAddStopsOnApplyError(t *testing.T) {
	t.Parallel()
	fake := &fakeNodeAddClient{applyErr: errors.New("store down")}
	var out strings.Builder
	err := runNodeAdd(context.Background(), fake, &out, &nodeAddOpts{name: "node-4", url: "10.0.0.1:7443"})
	if err == nil {
		t.Fatal("apply error swallowed")
	}
	if fake.tokenCalls != 0 {
		t.Fatal("token minted despite a failed apply")
	}
}
