package server

import (
	"context"
	"strings"
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
	if err := a.applyInstanceStatus(ctx, "node-1", update(klitev1.InstancePhase_INSTANCE_PHASE_READY)); err != nil {
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

	if err := a.applyInstanceStatus(ctx, "node-1", update(klitev1.InstancePhase_INSTANCE_PHASE_FAILED)); err != nil {
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

// Every server-materialized kind must decode cleanly and bounce off Apply
// with the pointed error — never reach the store, never panic the codec.
func TestApplyRejectsServerMaterializedKinds(t *testing.T) {
	t.Parallel()
	s := NewCluster(storetest.New(), NewCommandHub(), nil)
	docs := map[string]string{
		"Instance":          "apiVersion: klite/v1\nkind: Instance\nmetadata:\n  name: forged\n",
		"VIPAllocation":     "apiVersion: klite/v1\nkind: VIPAllocation\nmetadata:\n  name: forged.node-1\n",
		"IngressAllocation": "apiVersion: klite/v1\nkind: IngressAllocation\nmetadata:\n  name: forged.b-aa\n",
	}
	for kind, yaml := range docs {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			resp, err := s.Apply(context.Background(), &klitev1.ApplyRequest{Yaml: []byte(yaml)})
			if err != nil {
				t.Fatalf("Apply must answer per-object, got rpc error: %v", err)
			}
			got := resp.GetResults()[0].GetError()
			if !strings.Contains(got, "server-materialized") {
				t.Fatalf("result error = %q, want the server-materialized rejection", got)
			}
		})
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

// A node's report only writes statuses for instances scheduled to it. A
// report naming another node's instance is dropped whatever UID it carries,
// and the heartbeat still lands.
func TestReportStatusRejectsForeignNodeInstances(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	seedNode(t, st, "node-2", klitev1.NodePhase_NODE_PHASE_READY)
	seedInstance(t, st, "b-aa", "b", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	obj, _, err := st.Get(ctx, object.KindInstance, "b-aa")
	if err != nil {
		t.Fatal(err)
	}
	uid := obj.GetInstance().GetMeta().GetUid()
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok", Hub: NewCommandHub()})

	if _, err := a.ReportStatus(ctx, &klitev1.ReportStatusRequest{
		Node: "node-2",
		Instances: []*klitev1.InstanceStatusUpdate{{
			Name: "b-aa", Uid: uid,
			Status: &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_FAILED, InstanceIp: "10.9.9.9"},
		}},
	}); err != nil {
		t.Fatalf("the heartbeat must survive a rejected instance write: %v", err)
	}
	got, _, _ := st.Get(ctx, object.KindInstance, "b-aa")
	s := got.GetInstance().GetStatus()
	if s.GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_READY || s.GetInstanceIp() == "10.9.9.9" {
		t.Fatalf("status = %v, a foreign node's report must not land", s)
	}
	node, _, _ := st.Get(ctx, object.KindNode, "node-2")
	if node.GetNode().GetStatus().GetLastHeartbeatUnix() == 0 {
		t.Error("heartbeat was not stamped")
	}
}

// advertiseIP screens what agents report before it can reach EDS: hostnames,
// loopback, and unspecified addresses would each break or misroute every
// remote consumer.
func TestAdvertiseIPScreens(t *testing.T) {
	t.Parallel()
	tests := []struct{ addr, want string }{
		{"192.168.5.2", "192.168.5.2"},
		{"2001:db8::7", "2001:db8::7"},
		{"host.docker.internal", ""},
		{"127.0.0.1", ""},
		{"::1", ""},
		{"0.0.0.0", ""},
		{"::", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := advertiseIP("node-1", tt.addr); got != tt.want {
			t.Errorf("advertiseIP(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}
