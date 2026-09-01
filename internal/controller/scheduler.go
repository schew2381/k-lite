package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// MsgNoCapacity is the pending reason for a full cluster. The rollout
// machinery and the drain narrator match it to spot the drain-first fallback
// (ADR 0010).
const MsgNoCapacity = "no ready schedulable node with free capacity"

// scheduler binds unbound instances to nodes: filter on pin, readiness,
// cordon, and capacity, then pick the node running the fewest instances with
// names breaking ties (ADR 0012).
type scheduler struct {
	st store.Store
}

func (s *scheduler) reconcile(ctx context.Context) error {
	instObjs, _, err := s.st.List(ctx, object.KindInstance)
	if err != nil {
		return err
	}
	nodeObjs, _, err := s.st.List(ctx, object.KindNode)
	if err != nil {
		return err
	}
	wlObjs, _, err := s.st.List(ctx, object.KindWorkload)
	if err != nil {
		return err
	}

	counts := make(map[string]int, len(nodeObjs))
	var pending []*klitev1.Object
	for _, o := range instObjs {
		if n := o.GetInstance().GetSpec().GetNode(); n != "" {
			counts[n]++
		} else {
			pending = append(pending, o)
		}
	}
	pins := make(map[string]string)
	for _, o := range wlObjs {
		w := o.GetWorkload()
		if pin := w.GetSpec().GetNodeName(); pin != "" {
			pins[w.GetMeta().GetName()] = pin
		}
	}
	nodes := make([]*klitev1.Node, 0, len(nodeObjs))
	for _, o := range nodeObjs {
		nodes = append(nodes, o.GetNode())
	}

	var errs []error
	for _, o := range pending {
		inst := o.GetInstance()
		nodeName, reason := pickNode(pins[inst.GetSpec().GetWorkload()], nodes, counts)
		if nodeName == "" {
			errs = append(errs, s.explainPending(ctx, o, reason))
			continue
		}
		if err := s.bind(ctx, o, nodeName); err != nil {
			errs = append(errs, err)
			continue
		}
		counts[nodeName]++
	}
	return errors.Join(errs...)
}

// pickNode returns the chosen node name, or "" plus the reason nothing fit.
func pickNode(pin string, nodes []*klitev1.Node, counts map[string]int) (string, string) {
	var best *klitev1.Node
	bestCount := 0
	for _, n := range nodes {
		name := n.GetMeta().GetName()
		if pin != "" && name != pin {
			continue
		}
		if n.GetStatus().GetPhase() != klitev1.NodePhase_NODE_PHASE_READY {
			continue
		}
		if n.GetStatus().GetUnschedulable() {
			continue
		}
		if counts[name] >= int(n.GetSpec().GetMaxInstances()) {
			continue
		}
		if best == nil || counts[name] < bestCount ||
			(counts[name] == bestCount && name < best.GetMeta().GetName()) {
			best, bestCount = n, counts[name]
		}
	}
	if best == nil {
		if pin != "" {
			return "", fmt.Sprintf("pinned node %q is not schedulable", pin)
		}
		return "", MsgNoCapacity
	}
	return best.GetMeta().GetName(), ""
}

// bind writes spec.node, CAS-guarded so a concurrent writer wins cleanly and
// the next pass retries.
func (s *scheduler) bind(ctx context.Context, obj *klitev1.Object, node string) error {
	inst := obj.GetInstance()
	inst.Spec.Node = node
	if inst.Status == nil {
		inst.Status = &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_PENDING}
	}
	inst.Status.Message = ""
	_, err := s.st.Put(ctx, obj, inst.GetMeta().GetResourceVersion())
	switch {
	case errors.Is(err, store.ErrConflict):
		return nil
	case err != nil:
		return err
	}
	slog.Info("instance scheduled", "instance", inst.GetMeta().GetName(), "node", node)
	return nil
}

// explainPending records why an instance can't schedule, once per reason, so
// `klite get instances` shows it instead of a bare Pending.
func (s *scheduler) explainPending(ctx context.Context, obj *klitev1.Object, reason string) error {
	inst := obj.GetInstance()
	cur := inst.GetStatus()
	if cur.GetMessage() == reason && cur.GetPhase() == klitev1.InstancePhase_INSTANCE_PHASE_PENDING {
		return nil
	}
	if inst.Status == nil {
		inst.Status = &klitev1.InstanceStatus{}
	}
	inst.Status.Phase = klitev1.InstancePhase_INSTANCE_PHASE_PENDING
	inst.Status.Message = reason
	if _, err := s.st.Put(ctx, obj, inst.GetMeta().GetResourceVersion()); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}
