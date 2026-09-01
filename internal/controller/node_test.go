package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

func nodeObj(name string, phase klitev1.NodePhase, unschedulable bool, heartbeat int64, labels map[string]string) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
		Meta: &klitev1.Meta{Name: name, Labels: labels},
		Spec: &klitev1.NodeSpec{MaxInstances: 8},
		Status: &klitev1.NodeStatus{
			Phase: phase, Unschedulable: unschedulable, LastHeartbeatUnix: heartbeat,
		},
	}}}
}

// nodeNow is the fixed clock every node test runs at.
var nodeNow = time.Unix(5000, 0)

func nodeTestSetup(t *testing.T, objs ...*klitev1.Object) (*storetest.Memory, *nodeController) {
	t.Helper()
	st := storetest.New()
	for _, o := range objs {
		if _, err := st.Put(context.Background(), o, store.RevAny); err != nil {
			t.Fatal(err)
		}
	}
	return st, &nodeController{st: st, now: func() time.Time { return nodeNow }}
}

func TestNodeDrainCompletesToReadyCordoned(t *testing.T) {
	t.Parallel()
	st, c := nodeTestSetup(t,
		nodeObj("node-2", klitev1.NodePhase_NODE_PHASE_DRAINING, true, nodeNow.Unix(), nil))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.Get(context.Background(), object.KindNode, "node-2")
	if err != nil {
		t.Fatal(err)
	}
	s := got.GetNode().GetStatus()
	if s.GetPhase() != klitev1.NodePhase_NODE_PHASE_READY || !s.GetUnschedulable() {
		t.Fatalf("status = %v, want READY and still cordoned", s)
	}
}

func TestNodeDrainWaitsForInstances(t *testing.T) {
	t.Parallel()
	st, c := nodeTestSetup(t,
		nodeObj("node-2", klitev1.NodePhase_NODE_PHASE_DRAINING, true, nodeNow.Unix(), nil),
		&klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
			Meta:   &klitev1.Meta{Name: "b-aa"},
			Spec:   &klitev1.InstanceSpec{Workload: "b", Node: "node-2"},
			Status: &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_READY},
		}}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.Get(context.Background(), object.KindNode, "node-2")
	if got.GetNode().GetStatus().GetPhase() != klitev1.NodePhase_NODE_PHASE_DRAINING {
		t.Fatalf("phase = %v, want DRAINING while an instance remains", got.GetNode().GetStatus().GetPhase())
	}
}

func TestPendingDeleteNodeRemovedOnceEmpty(t *testing.T) {
	t.Parallel()
	labels := map[string]string{object.LabelPendingDelete: "true"}
	st, c := nodeTestSetup(t,
		nodeObj("node-2", klitev1.NodePhase_NODE_PHASE_DRAINING, true, nodeNow.Unix(), labels))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Get(context.Background(), object.KindNode, "node-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want the record gone", err)
	}
}

func TestPendingDeleteReassertsDrain(t *testing.T) {
	t.Parallel()
	labels := map[string]string{object.LabelPendingDelete: "true"}
	st, c := nodeTestSetup(t,
		// A heartbeat raced the drain and flipped the node READY.
		nodeObj("node-2", klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), labels),
		&klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
			Meta: &klitev1.Meta{Name: "b-aa"},
			Spec: &klitev1.InstanceSpec{Workload: "b", Node: "node-2"},
		}}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.Get(context.Background(), object.KindNode, "node-2")
	s := got.GetNode().GetStatus()
	if s.GetPhase() != klitev1.NodePhase_NODE_PHASE_DRAINING || !s.GetUnschedulable() {
		t.Fatalf("status = %v, want the drain re-asserted", s)
	}
}

func TestDrainingNodeGoesNotReadyWhenSilent(t *testing.T) {
	t.Parallel()
	st, c := nodeTestSetup(t,
		nodeObj("node-2", klitev1.NodePhase_NODE_PHASE_DRAINING, true, nodeNow.Add(-20*time.Second).Unix(), nil),
		&klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
			Meta: &klitev1.Meta{Name: "b-aa"},
			Spec: &klitev1.InstanceSpec{Workload: "b", Node: "node-2"},
		}}})
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.Get(context.Background(), object.KindNode, "node-2")
	if got.GetNode().GetStatus().GetPhase() != klitev1.NodePhase_NODE_PHASE_NOT_READY {
		t.Fatalf("phase = %v, want NOT_READY after silence mid-drain", got.GetNode().GetStatus().GetPhase())
	}
}
