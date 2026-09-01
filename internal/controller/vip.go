package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// vipPool is the control-plane-allocated VIP range (ADR 0006). klite-net
// rejects anything outside it.
var vipPool = netip.MustParsePrefix("10.44.64.0/18")

// vipController materializes one VIPAllocation per (Service, Node) pair and
// releases allocations whose service or node is gone. It runs leader-only, so
// CAS creates are only defending against its own retries.
type vipController struct {
	st store.Store
}

// AllocationName is the VIPAllocation object name for a (service, node) pair.
// The dot separator can't appear in either component (both are DNS labels).
func AllocationName(service, node string) string {
	return service + "." + node
}

func (c *vipController) reconcile(ctx context.Context) error {
	svcObjs, _, err := c.st.List(ctx, object.KindService)
	if err != nil {
		return err
	}
	nodeObjs, _, err := c.st.List(ctx, object.KindNode)
	if err != nil {
		return err
	}
	allocObjs, _, err := c.st.List(ctx, object.KindVIPAllocation)
	if err != nil {
		return err
	}

	want := map[string]*klitev1.VIPAllocationSpec{}
	for _, so := range svcObjs {
		svc := so.GetService().GetMeta().GetName()
		for _, no := range nodeObjs {
			node := no.GetNode().GetMeta().GetName()
			want[AllocationName(svc, node)] = &klitev1.VIPAllocationSpec{Service: svc, Node: node}
		}
	}

	used := map[netip.Addr]bool{}
	var errs []error
	for _, ao := range allocObjs {
		alloc := ao.GetVipAllocation()
		name := alloc.GetMeta().GetName()
		if _, ok := want[name]; !ok {
			errs = append(errs, c.release(ctx, name))
			continue
		}
		delete(want, name)
		if ip, err := netip.ParseAddr(alloc.GetSpec().GetVip()); err == nil {
			used[ip] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		errs = append(errs, c.allocate(ctx, want[name], used))
	}
	return errors.Join(errs...)
}

func (c *vipController) release(ctx context.Context, name string) error {
	if err := c.st.Delete(ctx, object.KindVIPAllocation, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	slog.Info("vip released", "allocation", name)
	return nil
}

func (c *vipController) allocate(ctx context.Context, spec *klitev1.VIPAllocationSpec, used map[netip.Addr]bool) error {
	ip, err := firstFreeVIP(used)
	if err != nil {
		return err
	}
	spec.Vip = ip.String()
	alloc := &klitev1.VIPAllocation{
		Meta: &klitev1.Meta{Name: AllocationName(spec.GetService(), spec.GetNode())},
		Spec: spec,
	}
	obj := &klitev1.Object{Kind: &klitev1.Object_VipAllocation{VipAllocation: alloc}}
	if _, err := c.st.Put(ctx, obj, store.RevCreate); err != nil {
		// Someone (an earlier leader life) beat us to the name; the next
		// pass reads their allocation.
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil
		}
		return err
	}
	used[ip] = true
	slog.Info("vip allocated", "service", spec.GetService(), "node", spec.GetNode(), "vip", spec.GetVip())
	return nil
}

// firstFreeVIP scans the pool in address order, skipping .0 and .255 hosts so
// nothing ever answers on an address a stray stack treats as broadcast.
func firstFreeVIP(used map[netip.Addr]bool) (netip.Addr, error) {
	for ip := vipPool.Addr().Next(); vipPool.Contains(ip); ip = ip.Next() {
		b := ip.As4()
		if b[3] == 0 || b[3] == 255 || used[ip] {
			continue
		}
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("vip pool %s exhausted", vipPool)
}
