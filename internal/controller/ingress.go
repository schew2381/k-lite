package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// The cross-node ingress range (ADR 0024): every node owns
// IngressPortsPerNode host ports starting at base + size*(index-1), and its
// donor publishes the whole slice at creation because Docker can't add
// published ports to a running container.
const (
	// DefaultIngressPortBase applies when klited's --ingress-port-base is
	// zero. A deliberate second cluster on the same machine must move it
	// (ADR 0030's knob pattern).
	DefaultIngressPortBase = 20000
	// IngressPortsPerNode is fixed, not a knob: allocator and donor must
	// agree on it or ports land outside the published slice.
	IngressPortsPerNode = 32
)

// IngressPortBase resolves the flag value, zero meaning the default.
func IngressPortBase(flag int32) int32 {
	if flag == 0 {
		return DefaultIngressPortBase
	}
	return flag
}

// IngressAllocationName is the IngressAllocation object name for a
// (service, instance) endpoint. Both components are DNS labels, so the dot
// can't collide.
func IngressAllocationName(service, instance string) string {
	return service + "." + instance
}

// ingressController materializes one IngressAllocation per (Service,
// Instance) endpoint from the owning node's port slice and releases
// allocations whose endpoint is gone. Like the VIP allocator (ADR 0022) it
// assumes double-leadership happens: create-only writes plus repair of
// duplicate, out-of-range, and wrong-node ports on every pass.
type ingressController struct {
	st   store.Store
	base int32
}

// nodeRange returns the node's half-open port slice [lo, hi). ok is false
// while the node has no index yet or the slice would leave the port space.
func (c *ingressController) nodeRange(index int32) (lo, hi int32, ok bool) {
	if index < 1 {
		return 0, 0, false
	}
	lo = c.base + IngressPortsPerNode*(index-1)
	hi = lo + IngressPortsPerNode
	if hi > 65536 {
		return 0, 0, false
	}
	return lo, hi, true
}

func (c *ingressController) reconcile(ctx context.Context) error {
	svcObjs, _, err := c.st.List(ctx, object.KindService)
	if err != nil {
		return err
	}
	instObjs, _, err := c.st.List(ctx, object.KindInstance)
	if err != nil {
		return err
	}
	nodeObjs, _, err := c.st.List(ctx, object.KindNode)
	if err != nil {
		return err
	}
	allocObjs, _, err := c.st.List(ctx, object.KindIngressAllocation)
	if err != nil {
		return err
	}

	index := make(map[string]int32, len(nodeObjs))
	for _, no := range nodeObjs {
		n := no.GetNode()
		index[n.GetMeta().GetName()] = n.GetStatus().GetNodeIndex()
	}
	want := c.wanted(svcObjs, instObjs, index)

	used := map[int32]bool{}
	var errs []error
	for _, ao := range allocObjs {
		errs = append(errs, c.reconcileAllocation(ctx, ao.GetIngressAllocation(), want, used, index))
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		errs = append(errs, c.allocate(ctx, want[name], used, index))
	}
	return errors.Join(errs...)
}

// wanted maps every (service, selected instance) pair to the allocation it
// should hold. Phase is ignored on purpose: the port is fixed at instance
// birth so the ingress listener exists by the time the endpoint turns Ready,
// and Ready<->Draining flips never churn it.
func (c *ingressController) wanted(svcObjs, instObjs []*klitev1.Object, index map[string]int32) map[string]*klitev1.IngressAllocationSpec {
	want := map[string]*klitev1.IngressAllocationSpec{}
	for _, so := range svcObjs {
		svc := so.GetService()
		for _, io := range instObjs {
			inst := io.GetInstance()
			node := inst.GetSpec().GetNode()
			if node == "" || !selects(svc, inst) {
				continue
			}
			if _, _, ok := c.nodeRange(index[node]); !ok {
				continue // no index yet; the registration write re-kicks us
			}
			name := IngressAllocationName(svc.GetMeta().GetName(), inst.GetMeta().GetName())
			want[name] = &klitev1.IngressAllocationSpec{
				Service:  svc.GetMeta().GetName(),
				Instance: inst.GetMeta().GetName(),
				Node:     node,
			}
		}
	}
	return want
}

// reconcileAllocation keeps, releases, or queues one existing allocation for
// reallocation. Every seen port is reserved, doomed or not, because the
// release may miss this pass and re-handing the port would mint the very
// duplicate this loop repairs (the VIP allocator's shape).
func (c *ingressController) reconcileAllocation(ctx context.Context, alloc *klitev1.IngressAllocation, want map[string]*klitev1.IngressAllocationSpec, used map[int32]bool, index map[string]int32) error {
	name := alloc.GetMeta().GetName()
	port := alloc.GetSpec().GetPort()
	lo, hi, rangeOK := c.nodeRange(index[alloc.GetSpec().GetNode()])
	valid := rangeOK && port >= lo && port < hi
	duplicate := valid && used[port]
	if port > 0 {
		used[port] = true
	}
	spec, wanted := want[name]
	delete(want, name)
	switch {
	case !wanted:
		return c.release(ctx, name, "endpoint gone")
	case spec.GetNode() != alloc.GetSpec().GetNode():
		// The name outlived its instance and a namesake landed elsewhere.
		want[name] = spec // reallocated below
		return c.release(ctx, name, "instance moved nodes")
	case !valid:
		want[name] = spec // reallocated below
		return c.release(ctx, name, "port outside the node's range")
	case duplicate:
		// List walks names in order, so the lexically-first holder keeps a
		// contested port and the repair converges on every pass.
		want[name] = spec // reallocated below
		return c.release(ctx, name, "duplicate port")
	}
	return nil
}

func (c *ingressController) release(ctx context.Context, name, reason string) error {
	err := c.st.Delete(ctx, object.KindIngressAllocation, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	slog.Info("ingress port released", "allocation", name, "reason", reason)
	return nil
}

func (c *ingressController) allocate(ctx context.Context, spec *klitev1.IngressAllocationSpec, used map[int32]bool, index map[string]int32) error {
	lo, hi, ok := c.nodeRange(index[spec.GetNode()])
	if !ok {
		return nil // wanted() screened this; a raced node delete lands here
	}
	port, err := firstFreePort(lo, hi, used)
	if err != nil {
		return fmt.Errorf("node %s: %w", spec.GetNode(), err)
	}
	// Reserve before the write settles: a lost create means the name holds
	// some other port, and reusing this candidate could mint a duplicate.
	used[port] = true
	spec.Port = port
	alloc := &klitev1.IngressAllocation{
		Meta: &klitev1.Meta{Name: IngressAllocationName(spec.GetService(), spec.GetInstance())},
		Spec: spec,
	}
	obj := &klitev1.Object{Kind: &klitev1.Object_IngressAllocation{IngressAllocation: alloc}}
	if _, err := c.st.Put(ctx, obj, store.RevCreate); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil // an earlier leader life won the name
		}
		return err
	}
	slog.Info("ingress port allocated",
		"service", spec.GetService(), "instance", spec.GetInstance(), "node", spec.GetNode(), "port", port)
	return nil
}

func firstFreePort(lo, hi int32, used map[int32]bool) (int32, error) {
	for p := lo; p < hi; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("ingress range %d-%d exhausted", lo, hi-1)
}
