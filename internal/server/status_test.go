package server

import (
	"context"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

// A controller-set DRAINING phase must survive agent reports that still say
// READY, and yield to FAILED (the container died mid-drain).
func TestApplyInstanceStatusKeepsDraining(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	seedInstance(t, st, "b-aa", "b", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING)
	obj, _, err := st.Get(ctx, object.KindInstance, "b-aa")
	if err != nil {
		t.Fatal(err)
	}
	uid := obj.GetInstance().GetMeta().GetUid()
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok", Hub: NewCommandHub()})

	update := func(phase klitev1.InstancePhase) *klitev1.InstanceStatusUpdate {
		return &klitev1.InstanceStatusUpdate{
			Name: "b-aa", Uid: uid,
			Status: &klitev1.InstanceStatus{Phase: phase, InstanceIp: "10.44.128.9", ContainerId: "ctr-1"},
		}
	}
	if err := a.applyInstanceStatus(ctx, update(klitev1.InstancePhase_INSTANCE_PHASE_READY)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.Get(ctx, object.KindInstance, "b-aa")
	s := got.GetInstance().GetStatus()
	if s.GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_DRAINING {
		t.Fatalf("phase = %v, want DRAINING kept over READY", s.GetPhase())
	}
	if s.GetInstanceIp() != "10.44.128.9" || s.GetContainerId() != "ctr-1" {
		t.Errorf("status = %v, want the rest of the report merged", s)
	}

	if err := a.applyInstanceStatus(ctx, update(klitev1.InstancePhase_INSTANCE_PHASE_FAILED)); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.Get(ctx, object.KindInstance, "b-aa")
	if got.GetInstance().GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED accepted mid-drain", got.GetInstance().GetStatus().GetPhase())
	}
}

func TestStampNodePreservesDraining(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_DRAINING)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok", Hub: NewCommandHub()})

	if err := a.stampNode(ctx, "node-2", ""); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.Get(ctx, object.KindNode, "node-2")
	s := got.GetNode().GetStatus()
	if s.GetPhase() != klitev1.NodePhase_NODE_PHASE_DRAINING {
		t.Fatalf("phase = %v, a heartbeat must not undo a drain", s.GetPhase())
	}
	if s.GetLastHeartbeatUnix() == 0 {
		t.Error("heartbeat must still be stamped")
	}
}

// The heartbeat carries the advertise address once the agent resolved it
// (ADR 0024): non-IP values are dropped, empty leaves the stored one alone.
func TestStampNodeAdvertiseAddress(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok", Hub: NewCommandHub()})

	addr := func() string {
		got, _, _ := st.Get(ctx, object.KindNode, "node-2")
		return got.GetNode().GetStatus().GetAdvertiseAddress()
	}
	if err := a.stampNode(ctx, "node-2", advertiseIP("node-2", "192.168.5.2")); err != nil {
		t.Fatal(err)
	}
	if addr() != "192.168.5.2" {
		t.Fatalf("advertise = %q, want 192.168.5.2", addr())
	}
	if err := a.stampNode(ctx, "node-2", advertiseIP("node-2", "host.docker.internal")); err != nil {
		t.Fatal(err)
	}
	if addr() != "192.168.5.2" {
		t.Fatalf("advertise = %q, a hostname must never replace the stored IP", addr())
	}
	if err := a.stampNode(ctx, "node-2", ""); err != nil {
		t.Fatal(err)
	}
	if addr() != "192.168.5.2" {
		t.Fatalf("advertise = %q, an empty report must not clear it", addr())
	}
}

// Deleting a node marks it pending-delete and draining; the record stays for
// the node controller to remove once its instances have left.
func TestDeleteNodeDrainsFirst(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	s := NewCluster(st, NewCommandHub(), nil)

	resp, err := s.Delete(ctx, &klitev1.DeleteRequest{Kind: "node", Name: "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetResults()[0].GetAction(); got != "draining (delete pending)" {
		t.Errorf("action = %q, want the drain-first action", got)
	}
	obj, _, err := st.Get(ctx, object.KindNode, "node-2")
	if err != nil {
		t.Fatalf("node record must remain until drained: %v", err)
	}
	node := obj.GetNode()
	if node.GetMeta().GetLabels()[object.LabelPendingDelete] != "true" {
		t.Error("pending-delete label missing")
	}
	nst := node.GetStatus()
	if !nst.GetUnschedulable() || nst.GetPhase() != klitev1.NodePhase_NODE_PHASE_DRAINING {
		t.Errorf("status = %v, want cordoned and DRAINING", nst)
	}

	// Deleting again is idempotent while the drain runs.
	resp, err = s.Delete(ctx, &klitev1.DeleteRequest{Kind: "node", Name: "node-2"})
	if err != nil || resp.GetResults()[0].GetAction() != "draining (delete pending)" {
		t.Fatalf("repeat delete = %v (%v), want the same drain-pending answer", resp.GetResults(), err)
	}
}
