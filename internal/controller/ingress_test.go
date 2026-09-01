package controller

import (
	"context"
	"strings"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

func indexedNodeObj(name string, index int32) *klitev1.Object {
	o := nodeObj(name, klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), nil)
	o.GetNode().GetStatus().NodeIndex = index
	return o
}

func instObj(name, node string, labels map[string]string) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Instance{
		Instance: inst(name, node, "10.44.128.10", klitev1.InstancePhase_INSTANCE_PHASE_PENDING, labels, 0),
	}}
}

func ingressAllocObj(service, instance, node string, port int32) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_IngressAllocation{IngressAllocation: &klitev1.IngressAllocation{
		Meta: &klitev1.Meta{Name: IngressAllocationName(service, instance)},
		Spec: &klitev1.IngressAllocationSpec{Service: service, Instance: instance, Node: node, Port: port},
	}}}
}

func ingressSetup(t *testing.T, objs ...*klitev1.Object) (*storetest.Memory, *ingressController) {
	t.Helper()
	st := storetest.New()
	for _, o := range objs {
		if _, err := st.Put(context.Background(), o, store.RevAny); err != nil {
			t.Fatal(err)
		}
	}
	return st, &ingressController{st: st, base: DefaultIngressPortBase}
}

// ingressPorts reads back every allocation as name -> (node, port).
func ingressPorts(t *testing.T, st *storetest.Memory) map[string]*klitev1.IngressAllocationSpec {
	t.Helper()
	objs, _, err := st.List(context.Background(), object.KindIngressAllocation)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]*klitev1.IngressAllocationSpec, len(objs))
	for _, o := range objs {
		a := o.GetIngressAllocation()
		out[a.GetMeta().GetName()] = a.GetSpec()
	}
	return out
}

// Every (service, selected instance) pair gets a port from the owning
// node's slice, regardless of phase; unselected instances get nothing.
func TestIngressAllocatesPerEndpointFromNodeRange(t *testing.T) {
	t.Parallel()
	b := map[string]string{"app": "b"}
	st, c := ingressSetup(t,
		serviceObj("b"),
		indexedNodeObj("node-1", 1), indexedNodeObj("node-2", 2),
		instObj("b-aa", "node-1", b), instObj("b-ab", "node-1", b), instObj("b-ac", "node-2", b),
		instObj("x-aa", "node-1", map[string]string{"app": "x"}))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := ingressPorts(t, st)
	if len(got) != 3 {
		t.Fatalf("allocations = %v, want one per selected instance", got)
	}
	seen := map[int32]string{}
	for name, spec := range got {
		lo, hi, ok := c.nodeRange(map[string]int32{"node-1": 1, "node-2": 2}[spec.GetNode()])
		if !ok || spec.GetPort() < lo || spec.GetPort() >= hi {
			t.Errorf("%s holds port %d, outside its node slice [%d,%d)", name, spec.GetPort(), lo, hi)
		}
		if prev, dup := seen[spec.GetPort()]; dup {
			t.Errorf("port %d handed to both %s and %s", spec.GetPort(), prev, name)
		}
		seen[spec.GetPort()] = name
	}
	if _, ok := got[IngressAllocationName("b", "x-aa")]; ok {
		t.Error("unselected instance got an allocation")
	}
}

// A gone endpoint releases its port, and the port is not re-handed in the
// same pass that frees it.
func TestIngressReleasesStaleAndReservesItsPort(t *testing.T) {
	t.Parallel()
	st, c := ingressSetup(t,
		serviceObj("b"),
		indexedNodeObj("node-1", 1),
		instObj("b-aa", "node-1", map[string]string{"app": "b"}),
		ingressAllocObj("b", "b-gone", "node-1", 20000))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := ingressPorts(t, st)
	if _, ok := got[IngressAllocationName("b", "b-gone")]; ok {
		t.Fatal("stale allocation survived reconcile")
	}
	spec := got[IngressAllocationName("b", "b-aa")]
	if spec.GetPort() == 0 || spec.GetPort() == 20000 {
		t.Fatalf("b.b-aa port = %d, want a fresh port, not the just-released one", spec.GetPort())
	}
}

// Repairs: a duplicate port moves the lexically-second holder, a port
// outside the node's slice reallocates, and a second pass changes nothing.
func TestIngressRepairsDuplicateAndOutOfRange(t *testing.T) {
	t.Parallel()
	b := map[string]string{"app": "b"}
	st, c := ingressSetup(t,
		serviceObj("b"),
		indexedNodeObj("node-1", 1),
		instObj("b-aa", "node-1", b), instObj("b-ab", "node-1", b), instObj("b-ac", "node-1", b),
		ingressAllocObj("b", "b-aa", "node-1", 20005),
		ingressAllocObj("b", "b-ab", "node-1", 20005),
		ingressAllocObj("b", "b-ac", "node-1", 19999))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := ingressPorts(t, st)
	if p := got[IngressAllocationName("b", "b-aa")].GetPort(); p != 20005 {
		t.Fatalf("first holder moved to %d, must keep the contested port", p)
	}
	moved := got[IngressAllocationName("b", "b-ab")].GetPort()
	if moved == 20005 || moved < 20000 || moved >= 20032 {
		t.Fatalf("duplicate holder now at %d, want a fresh in-slice port", moved)
	}
	fixed := got[IngressAllocationName("b", "b-ac")].GetPort()
	if fixed < 20000 || fixed >= 20032 {
		t.Fatalf("out-of-range holder now at %d, want inside [20000,20032)", fixed)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again := ingressPorts(t, st); again[IngressAllocationName("b", "b-ab")].GetPort() != moved {
		t.Fatal("repair must be stable across passes")
	}
}

// No allocation happens while the node has no index: registration assigns
// it, and that write re-kicks the loop.
func TestIngressWaitsForNodeIndex(t *testing.T) {
	t.Parallel()
	st, c := ingressSetup(t,
		serviceObj("b"),
		indexedNodeObj("node-1", 0),
		instObj("b-aa", "node-1", map[string]string{"app": "b"}))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ingressPorts(t, st); len(got) != 0 {
		t.Fatalf("allocations = %v, want none before the node holds an index", got)
	}
}

func TestIngressRangeExhausted(t *testing.T) {
	t.Parallel()
	used := map[int32]bool{}
	for p := int32(20000); p < 20032; p++ {
		used[p] = true
	}
	if _, err := firstFreePort(20000, 20032, used); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("err = %v, want range exhaustion", err)
	}
}

func TestIngressNodeRangeBounds(t *testing.T) {
	t.Parallel()
	c := &ingressController{base: 20000}
	if lo, hi, ok := c.nodeRange(1); !ok || lo != 20000 || hi != 20032 {
		t.Fatalf("index 1 range = [%d,%d) ok=%v, want [20000,20032)", lo, hi, ok)
	}
	if lo, hi, ok := c.nodeRange(3); !ok || lo != 20064 || hi != 20096 {
		t.Fatalf("index 3 range = [%d,%d) ok=%v, want [20064,20096)", lo, hi, ok)
	}
	if _, _, ok := c.nodeRange(0); ok {
		t.Fatal("index 0 must have no range")
	}
	if _, _, ok := c.nodeRange(2000); ok {
		t.Fatal("a range past 65535 must be refused")
	}
}

// A release decided on a stale list must not remove an allocation another
// leader life re-minted in the meantime: that would churn a port the
// instance already advertises. The revision pin turns it into a no-op.
func TestIngressStaleReleaseSparesReMintedAllocation(t *testing.T) {
	t.Parallel()
	st, c := ingressSetup(t, ingressAllocObj("b", "b-aa", "node-1", 20000))
	staleObjs, _, err := st.List(context.Background(), object.KindIngressAllocation)
	if err != nil {
		t.Fatal(err)
	}
	name := IngressAllocationName("b", "b-aa")
	if err := st.Delete(context.Background(), object.KindIngressAllocation, name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), ingressAllocObj("b", "b-aa", "node-1", 20007), store.RevCreate); err != nil {
		t.Fatal(err)
	}
	staleRev := staleObjs[0].GetIngressAllocation().GetMeta().GetResourceVersion()
	if err := c.release(context.Background(), name, staleRev, "test"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := ingressPorts(t, st)
	if got[name].GetPort() != 20007 {
		t.Fatalf("allocation = %v, the re-minted port must survive a stale release", got[name])
	}
}

// A negative base (int32 truncation of a fat-fingered flag) must allocate
// nothing rather than mint negative ports, which xds validation would reject
// on every node's snapshot.
func TestIngressRefusesNegativeBase(t *testing.T) {
	t.Parallel()
	st, c := ingressSetup(t,
		serviceObj("b"),
		indexedNodeObj("node-1", 1),
		instObj("b-aa", "node-1", map[string]string{"app": "b"}))
	c.base = -20000
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ingressPorts(t, st); len(got) != 0 {
		t.Fatalf("allocations = %v, want none from a negative base", got)
	}
	if _, _, ok := c.nodeRange(1); ok {
		t.Fatal("a negative base must yield no range")
	}
}
