package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

// These tests pin ADR 0042's ownership split, after incident 113013.
// Applying YAML must never cost a node its server-set status. A record that
// lost its index anyway must get it back on its agent's next heartbeat,
// before freeNodeIndex hands the hole to a joiner whose donor then fights
// the incumbent's over one address.

func nodeDoc(name string, maxInstances int) []byte {
	return fmt.Appendf(nil,
		"apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: %s\n  labels:\n    zone: local\nspec:\n  maxInstances: %d\n",
		name, maxInstances)
}

func workloadDoc(name string, replicas int) []byte {
	return fmt.Appendf(nil, `apiVersion: klite/v1
kind: Workload
metadata:
  name: %s
  labels:
    app: %s
spec:
  replicas: %d
  template:
    labels:
      app: %s
    containers:
      - name: web
        image: traefik/whoami:v1.10
        ports:
          - containerPort: 80
`, name, name, replicas, name)
}

// applyOneDoc applies a single-document YAML and returns its result.
func applyOneDoc(t *testing.T, s *Cluster, doc []byte) *klitev1.ApplyResult {
	t.Helper()
	resp, err := s.Apply(context.Background(), &klitev1.ApplyRequest{Yaml: doc})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	res := resp.GetResults()[0]
	if res.GetError() != "" {
		t.Fatalf("apply %s/%s: %s", res.GetKind(), res.GetName(), res.GetError())
	}
	return res
}

func nodeStatus(t *testing.T, st store.Store, name string) *klitev1.NodeStatus {
	t.Helper()
	obj, _, err := st.Get(context.Background(), object.KindNode, name)
	if err != nil {
		t.Fatal(err)
	}
	return obj.GetNode().GetStatus()
}

// declareAndRegister applies the node's YAML and registers its agent,
// returning the assigned index.
func declareAndRegister(t *testing.T, s *Cluster, a *Agent, name string) int32 {
	t.Helper()
	applyOneDoc(t, s, nodeDoc(name, 32))
	resp, err := a.Register(context.Background(), &klitev1.RegisterRequest{Node: name, ClusterToken: "tok"})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return resp.GetNet().GetNodeIndex()
}

// Re-applying a node's YAML is how an operator cancels a pending delete
// (ADR 0033), so it happens exactly when status matters most. The stored
// index, phase, and heartbeat must ride through both a no-op and a real
// spec change.
func TestApplyOverRegisteredNodePreservesStatus(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	s := NewCluster(st, nil, nil)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok"})
	if got := declareAndRegister(t, s, a, "node-1"); got != 1 {
		t.Fatalf("first index = %d, want 1", got)
	}
	before := nodeStatus(t, st, "node-1")
	if before.GetLastHeartbeatUnix() == 0 || before.GetPhase() != klitev1.NodePhase_NODE_PHASE_READY {
		t.Fatalf("register left status %v, want READY with a heartbeat", before)
	}

	// The status carried forward must not defeat the no-op check on
	// identical YAML, or every re-apply would burn a revision.
	if res := applyOneDoc(t, s, nodeDoc("node-1", 32)); res.GetAction() != "unchanged" {
		t.Errorf("no-op re-apply action = %q, want unchanged", res.GetAction())
	}

	if res := applyOneDoc(t, s, nodeDoc("node-1", 16)); res.GetAction() != "updated" {
		t.Errorf("spec-change action = %q, want updated", res.GetAction())
	}
	after := nodeStatus(t, st, "node-1")
	if after.GetNodeIndex() != 1 || after.GetPhase() != before.GetPhase() ||
		after.GetLastHeartbeatUnix() != before.GetLastHeartbeatUnix() {
		t.Fatalf("status after re-apply = %v, want index/phase/heartbeat of %v", after, before)
	}
	obj, _, _ := st.Get(context.Background(), object.KindNode, "node-1")
	if got := obj.GetNode().GetSpec().GetMaxInstances(); got != 16 {
		t.Errorf("maxInstances = %d, the spec change must still land", got)
	}
}

// A client that round-trips `klite get -o yaml` sends status back. The
// server must keep its own, and the action string must say so.
func TestApplyIgnoresClientStatus(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	s := NewCluster(st, nil, nil)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok"})
	declareAndRegister(t, s, a, "node-1")

	forged := []byte("apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: node-1\n  labels:\n    zone: local\nspec:\n  maxInstances: 32\nstatus:\n  nodeIndex: 9\n")
	res := applyOneDoc(t, s, forged)
	if !strings.Contains(res.GetAction(), "(client status ignored)") {
		t.Errorf("action = %q, want the client-status note", res.GetAction())
	}
	if got := nodeStatus(t, st, "node-1").GetNodeIndex(); got != 1 {
		t.Fatalf("index = %d after a forged status, want the stored 1", got)
	}

	// On create there is nothing stored to keep, so the forged status must
	// simply vanish rather than seed the record.
	fresh := []byte("apiVersion: klite/v1\nkind: Node\nmetadata:\n  name: node-9\nspec:\n  maxInstances: 32\nstatus:\n  nodeIndex: 7\n")
	res = applyOneDoc(t, s, fresh)
	if !strings.Contains(res.GetAction(), "created") || !strings.Contains(res.GetAction(), "(client status ignored)") {
		t.Errorf("action = %q, want created with the client-status note", res.GetAction())
	}
	if got := nodeStatus(t, st, "node-9"); got.GetNodeIndex() != 0 {
		t.Fatalf("fresh node status = %v, want no index seeded by the client", got)
	}
}

// Workload status is the controllers' running tally. A re-applied manifest
// changes the spec and nothing else.
func TestApplyOverWorkloadPreservesStatus(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	s := NewCluster(st, nil, nil)
	applyOneDoc(t, s, workloadDoc("web", 2))

	// Mimic the workload controller's write.
	obj, rev, err := st.Get(ctx, object.KindWorkload, "web")
	if err != nil {
		t.Fatal(err)
	}
	obj.GetWorkload().Status = &klitev1.WorkloadStatus{ReadyInstances: 2, TotalInstances: 2, TemplateHash: "abc123"}
	if _, err := st.Put(ctx, obj, rev); err != nil {
		t.Fatal(err)
	}

	if res := applyOneDoc(t, s, workloadDoc("web", 2)); res.GetAction() != "unchanged" {
		t.Errorf("no-op re-apply action = %q, want unchanged", res.GetAction())
	}
	if res := applyOneDoc(t, s, workloadDoc("web", 3)); res.GetAction() != "updated" {
		t.Errorf("scale-by-apply action = %q, want updated", res.GetAction())
	}
	got, _, _ := st.Get(ctx, object.KindWorkload, "web")
	status := got.GetWorkload().GetStatus()
	if status.GetReadyInstances() != 2 || status.GetTotalInstances() != 2 || status.GetTemplateHash() != "abc123" {
		t.Fatalf("workload status = %v, want the controller's tally intact", status)
	}
}

// Replay the churn that broke the live cluster: re-apply every node's YAML,
// then let a new node join. The joiner must get a fresh index, never one a
// live agent still runs.
func TestRegisterAfterReapplyIssuesFreshIndex(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	s := NewCluster(st, nil, nil)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok"})
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("node-%d", i)
		if got := declareAndRegister(t, s, a, name); got != int32(i) {
			t.Fatalf("%s index = %d, want %d", name, got, i)
		}
	}
	for i := 1; i <= 3; i++ {
		applyOneDoc(t, s, nodeDoc(fmt.Sprintf("node-%d", i), 32))
	}
	if got := declareAndRegister(t, s, a, "node-4"); got != 4 {
		t.Fatalf("node-4 index = %d after re-apply churn, want 4", got)
	}
	for i := 1; i <= 3; i++ {
		if got := nodeStatus(t, st, fmt.Sprintf("node-%d", i)).GetNodeIndex(); got != int32(i) {
			t.Errorf("node-%d index = %d after churn, want %d", i, got, i)
		}
	}
}

// A record recreated without its index (delete finished, then the YAML came
// back) gets it restored by the agent's next heartbeat, so a later joiner
// sees the index as held.
func TestReportStatusRestoresWipedIndex(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	s := NewCluster(st, nil, nil)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok"})
	for i := 1; i <= 3; i++ {
		declareAndRegister(t, s, a, fmt.Sprintf("node-%d", i))
	}

	// Deal the incident's damage by hand: the drain emptied node-2, the
	// controller removed the record, and the re-applied YAML recreated it
	// statusless while the agent kept running index 2.
	if err := st.Delete(ctx, object.KindNode, "node-2"); err != nil {
		t.Fatal(err)
	}
	applyOneDoc(t, s, nodeDoc("node-2", 32))
	if got := nodeStatus(t, st, "node-2").GetNodeIndex(); got != 0 {
		t.Fatalf("recreated record index = %d, want 0 before the heal", got)
	}

	if _, err := a.ReportStatus(ctx, &klitev1.ReportStatusRequest{Node: "node-2", NodeIndex: 2}); err != nil {
		t.Fatal(err)
	}
	healed := nodeStatus(t, st, "node-2")
	if healed.GetNodeIndex() != 2 {
		t.Fatalf("index = %d after the heartbeat, want 2 restored", healed.GetNodeIndex())
	}
	if healed.GetPhase() != klitev1.NodePhase_NODE_PHASE_READY || healed.GetLastHeartbeatUnix() == 0 {
		t.Errorf("status = %v, the heartbeat must still stamp READY", healed)
	}
	if got := declareAndRegister(t, s, a, "node-4"); got != 4 {
		t.Fatalf("node-4 index = %d after the heal, want 4 (2 is held again)", got)
	}
}

// The heal is restore-only. A held index stays with its holder, and a
// mismatched report never rewrites a set one. No store write can settle
// which of two live agents owns the infra, so the server just says so.
func TestReportStatusNeverStealsOrOverwritesIndex(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	s := NewCluster(st, nil, nil)
	a := NewAgent(&AgentConfig{Store: st, ClusterToken: "tok"})
	for i := 1; i <= 3; i++ {
		declareAndRegister(t, s, a, fmt.Sprintf("node-%d", i))
	}
	if err := st.Delete(ctx, object.KindNode, "node-2"); err != nil {
		t.Fatal(err)
	}
	applyOneDoc(t, s, nodeDoc("node-2", 32))

	// node-4 joins before node-2's heartbeat lands and takes the freed 2,
	// replaying the incident one register ahead of the heal.
	if got := declareAndRegister(t, s, a, "node-4"); got != 2 {
		t.Fatalf("node-4 index = %d, want 2 (the hole the wipe opened)", got)
	}
	if _, err := a.ReportStatus(ctx, &klitev1.ReportStatusRequest{Node: "node-2", NodeIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if got := nodeStatus(t, st, "node-2").GetNodeIndex(); got != 0 {
		t.Fatalf("node-2 index = %d, the heal must not steal a held index", got)
	}
	if got := nodeStatus(t, st, "node-4").GetNodeIndex(); got != 2 {
		t.Fatalf("node-4 index = %d, the holder must keep it", got)
	}

	// A set index outranks any report.
	if _, err := a.ReportStatus(ctx, &klitev1.ReportStatusRequest{Node: "node-1", NodeIndex: 5}); err != nil {
		t.Fatal(err)
	}
	if got := nodeStatus(t, st, "node-1").GetNodeIndex(); got != 1 {
		t.Fatalf("node-1 index = %d after a mismatched report, want 1 kept", got)
	}
}
