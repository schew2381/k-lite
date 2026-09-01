package controller

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

func serviceObj(name string) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Service{Service: &klitev1.Service{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.ServiceSpec{Selector: map[string]string{"app": name}, Port: 80, TargetPort: 8080},
	}}}
}

func allocationObj(service, node, vip string) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_VipAllocation{VipAllocation: &klitev1.VIPAllocation{
		Meta: &klitev1.Meta{Name: AllocationName(service, node)},
		Spec: &klitev1.VIPAllocationSpec{Service: service, Node: node, Vip: vip},
	}}}
}

func vipSetup(t *testing.T, objs ...*klitev1.Object) (*storetest.Memory, *vipController) {
	t.Helper()
	st := storetest.New()
	for _, o := range objs {
		if _, err := st.Put(context.Background(), o, store.RevAny); err != nil {
			t.Fatal(err)
		}
	}
	return st, &vipController{st: st}
}

// allocations reads back every allocation as name -> vip.
func allocations(t *testing.T, st *storetest.Memory) map[string]string {
	t.Helper()
	objs, _, err := st.List(context.Background(), object.KindVIPAllocation)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(objs))
	for _, o := range objs {
		a := o.GetVipAllocation()
		out[a.GetMeta().GetName()] = a.GetSpec().GetVip()
	}
	return out
}

func TestVIPAllocatesEveryServiceNodePair(t *testing.T) {
	t.Parallel()
	st, c := vipSetup(t,
		serviceObj("a"), serviceObj("b"),
		nodeObj("node-1", klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), nil),
		nodeObj("node-2", klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), nil))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := allocations(t, st)
	if len(got) != 4 {
		t.Fatalf("allocations = %v, want one per (service, node) pair", got)
	}
	seen := map[string]string{}
	for name, vip := range got {
		ip, err := netip.ParseAddr(vip)
		if err != nil || !vipPool.Contains(ip) {
			t.Errorf("allocation %s holds %q, want an address inside %s", name, vip, vipPool)
		}
		if prev, ok := seen[vip]; ok {
			t.Errorf("vip %s handed to both %s and %s", vip, prev, name)
		}
		seen[vip] = name
	}
}

func TestVIPReleasesStaleAndReservesItsAddress(t *testing.T) {
	t.Parallel()
	// The stale allocation's service is gone, and a live pair needs a VIP.
	// The freed address must not be handed out in the pass that deletes it.
	st, c := vipSetup(t,
		serviceObj("b"),
		nodeObj("node-1", klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), nil),
		allocationObj("gone", "node-1", "10.44.64.1"))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := allocations(t, st)
	if _, ok := got[AllocationName("gone", "node-1")]; ok {
		t.Fatal("stale allocation survived reconcile")
	}
	if vip := got[AllocationName("b", "node-1")]; vip == "" || vip == "10.44.64.1" {
		t.Fatalf("b.node-1 vip = %q, want a fresh address, not the just-released one", vip)
	}
}

// TestVIPDuplicateRepaired: two leader lives can each create-only a different
// allocation name holding the same address. The reconcile must keep exactly
// one holder (the lexically-first) and move the other to a fresh VIP.
func TestVIPDuplicateRepaired(t *testing.T) {
	t.Parallel()
	st, c := vipSetup(t,
		serviceObj("b"), serviceObj("c"),
		nodeObj("node-1", klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), nil),
		allocationObj("b", "node-1", "10.44.64.1"),
		allocationObj("c", "node-1", "10.44.64.1"))
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := allocations(t, st)
	if got[AllocationName("b", "node-1")] != "10.44.64.1" {
		t.Fatalf("b.node-1 = %q, the first holder must keep the contested VIP", got[AllocationName("b", "node-1")])
	}
	cVIP := got[AllocationName("c", "node-1")]
	if cVIP == "" || cVIP == "10.44.64.1" {
		t.Fatalf("c.node-1 = %q, want a different in-pool address", cVIP)
	}
	// A second pass must change nothing.
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again := allocations(t, st); again[AllocationName("c", "node-1")] != cVIP {
		t.Fatalf("c.node-1 moved again to %q, repair must be stable", again[AllocationName("c", "node-1")])
	}
}

func TestVIPOutOfPoolReallocated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vip  string
	}{
		{"unparsable", "not-an-ip"},
		{"outside the pool", "192.168.1.5"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, c := vipSetup(t,
				serviceObj("b"),
				nodeObj("node-1", klitev1.NodePhase_NODE_PHASE_READY, false, nodeNow.Unix(), nil),
				allocationObj("b", "node-1", tt.vip))
			if err := c.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			vip := allocations(t, st)[AllocationName("b", "node-1")]
			ip, err := netip.ParseAddr(vip)
			if err != nil || !vipPool.Contains(ip) {
				t.Fatalf("vip = %q, want a fresh in-pool address", vip)
			}
		})
	}
}

// TestVIPAllocateLostCreateReservesCandidate: when the create-only Put loses
// to a concurrent leader, the candidate address must still count as used so
// the same pass can't hand it to the next pair.
func TestVIPAllocateLostCreateReservesCandidate(t *testing.T) {
	t.Parallel()
	st, c := vipSetup(t, allocationObj("b", "node-1", "10.44.64.9"))
	used := map[netip.Addr]bool{}
	if err := c.allocate(context.Background(), &klitev1.VIPAllocationSpec{Service: "b", Node: "node-1"}, used); err != nil {
		t.Fatalf("allocate against an existing name: %v", err)
	}
	if !used[netip.MustParseAddr("10.44.64.1")] {
		t.Fatal("losing candidate 10.44.64.1 was not reserved")
	}
	if err := c.allocate(context.Background(), &klitev1.VIPAllocationSpec{Service: "c", Node: "node-1"}, used); err != nil {
		t.Fatal(err)
	}
	if vip := allocations(t, st)[AllocationName("c", "node-1")]; vip == "10.44.64.1" {
		t.Fatal("next pair received the reserved candidate address")
	}
}

func TestFirstFreeVIP(t *testing.T) {
	t.Parallel()
	used := map[netip.Addr]bool{}
	ip, err := firstFreeVIP(used)
	if err != nil || ip.String() != "10.44.64.1" {
		t.Fatalf("first vip = %v, %v", ip, err)
	}
	used[ip] = true
	ip, err = firstFreeVIP(used)
	if err != nil || ip.String() != "10.44.64.2" {
		t.Fatalf("second vip = %v, %v", ip, err)
	}
	// Fill 10.44.64.0/24's usable hosts. Addresses .0 and .255 are never
	// handed out, so the next pick crosses into 10.44.65.x.
	for i := 1; i <= 254; i++ {
		used[netip.AddrFrom4([4]byte{10, 44, 64, byte(i)})] = true
	}
	ip, err = firstFreeVIP(used)
	if err != nil || ip.String() != "10.44.65.1" {
		t.Fatalf("post-boundary vip = %v, %v", ip, err)
	}
}

func TestFirstFreeVIPExhausted(t *testing.T) {
	t.Parallel()
	used := map[netip.Addr]bool{}
	for ip := vipPool.Addr(); vipPool.Contains(ip); ip = ip.Next() {
		used[ip] = true
	}
	if _, err := firstFreeVIP(used); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("err = %v, want pool exhaustion", err)
	}
}
