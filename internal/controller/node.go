package controller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

const (
	// heartbeatTimeout is how long a node may stay silent before it turns
	// NOT_READY.
	heartbeatTimeout = 15 * time.Second
	// evacuateAfter is the extra grace a NOT_READY node gets before its
	// instances are deleted for the workload controller to recreate and the
	// scheduler to place elsewhere.
	evacuateAfter = 30 * time.Second
)

// nodeController turns silent nodes NOT_READY, evacuates them after a grace
// window, keeps per-node instance counts current, and finishes drains. A
// DRAINING node with no instances left flips back to READY (still cordoned),
// or disappears when its deletion is pending (ADR 0010).
type nodeController struct {
	st  store.Store
	now func() time.Time
}

func (c *nodeController) reconcile(ctx context.Context) error {
	nodeObjs, _, err := c.st.List(ctx, object.KindNode)
	if err != nil {
		return err
	}
	instObjs, _, err := c.st.List(ctx, object.KindInstance)
	if err != nil {
		return err
	}
	counts := make(map[string]int, len(nodeObjs))
	for _, o := range instObjs {
		if n := o.GetInstance().GetSpec().GetNode(); n != "" {
			counts[n]++
		}
	}
	var errs []error
	for _, o := range nodeObjs {
		name := o.GetNode().GetMeta().GetName()
		errs = append(errs, c.reconcileNode(ctx, o, counts[name], instObjs))
	}
	return errors.Join(errs...)
}

func (c *nodeController) reconcileNode(ctx context.Context, obj *klitev1.Object, count int, instObjs []*klitev1.Object) error {
	node := obj.GetNode()
	name := node.GetMeta().GetName()
	if node.Status == nil {
		node.Status = &klitev1.NodeStatus{}
	}
	st := node.Status
	pendingDelete := node.GetMeta().GetLabels()[object.LabelPendingDelete] == "true"
	if pendingDelete && count == 0 {
		// Pinned to the listed revision because a re-apply cancels a pending
		// delete (ADR 0033): a lagging pass that still sees the label must
		// not take out the re-declared record. Conflict and absence both mean
		// the next pass decides on fresh state.
		err := c.st.DeleteIfRevision(ctx, object.KindNode, name, node.GetMeta().GetResourceVersion())
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
			return nil
		case err != nil:
			return err
		}
		slog.Info("node deleted after drain", "node", name)
		return nil
	}
	// A node that never heartbeated has nothing to time out, so it stays
	// in whatever phase it has until an agent registers.
	silence := time.Duration(0)
	if st.GetLastHeartbeatUnix() > 0 {
		silence = time.Duration(c.now().Unix()-st.GetLastHeartbeatUnix()) * time.Second
	}

	dirty := phaseTransition(st, name, silence, count, pendingDelete)
	if int32(count) != st.GetInstanceCount() {
		st.InstanceCount = int32(count)
		dirty = true
	}
	if dirty {
		// A lost revision race resolves on the next pass.
		if _, err := c.st.Put(ctx, obj, node.GetMeta().GetResourceVersion()); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	if st.GetPhase() == klitev1.NodePhase_NODE_PHASE_NOT_READY && silence > heartbeatTimeout+evacuateAfter {
		return c.evacuate(ctx, name, instObjs)
	}
	return nil
}

// phaseTransition applies at most one phase move and reports whether it
// changed anything.
func phaseTransition(st *klitev1.NodeStatus, name string, silence time.Duration, count int, pendingDelete bool) bool {
	drainable := st.GetPhase() == klitev1.NodePhase_NODE_PHASE_READY || st.GetPhase() == klitev1.NodePhase_NODE_PHASE_DRAINING
	switch {
	case silence > heartbeatTimeout && drainable:
		st.Phase = klitev1.NodePhase_NODE_PHASE_NOT_READY
		slog.Info("node lost", "node", name, "silence", silence)
	case pendingDelete && (st.GetPhase() == klitev1.NodePhase_NODE_PHASE_READY || !st.GetUnschedulable()):
		// Re-assert the drain a heartbeat or re-apply may have undone.
		st.Phase = klitev1.NodePhase_NODE_PHASE_DRAINING
		st.Unschedulable = true
	case st.GetPhase() == klitev1.NodePhase_NODE_PHASE_DRAINING && count == 0:
		// The drain is done, and the cordon outlives it (Nomad precedent).
		st.Phase = klitev1.NodePhase_NODE_PHASE_READY
		slog.Info("node drained", "node", name)
	default:
		return false
	}
	return true
}

// evacuate deletes a dead node's instance objects, each pinned to the
// revision this pass listed, so a lagging leader can't evacuate a namesake
// recreated after its list. The workload controller recreates the deleted
// instances, the scheduler places them on live nodes, and the dead node's
// containers wait for its agent to return and clean up.
func (c *nodeController) evacuate(ctx context.Context, node string, instObjs []*klitev1.Object) error {
	var errs []error
	for _, o := range instObjs {
		inst := o.GetInstance()
		if inst.GetSpec().GetNode() != node {
			continue
		}
		name := inst.GetMeta().GetName()
		err := c.st.DeleteIfRevision(ctx, object.KindInstance, name, inst.GetMeta().GetResourceVersion())
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
			continue // gone or moved since the list, so the next pass re-observes
		case err != nil:
			errs = append(errs, err)
			continue
		}
		slog.Info("instance evacuated from lost node", "instance", name, "node", node)
	}
	return errors.Join(errs...)
}
