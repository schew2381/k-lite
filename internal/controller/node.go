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
// window, and keeps per-node instance counts current.
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
	// A node that never heartbeated has nothing to time out, so it stays
	// in whatever phase it has until an agent registers.
	silence := time.Duration(0)
	if st.GetLastHeartbeatUnix() > 0 {
		silence = time.Duration(c.now().Unix()-st.GetLastHeartbeatUnix()) * time.Second
	}

	dirty := false
	if silence > heartbeatTimeout && st.GetPhase() == klitev1.NodePhase_NODE_PHASE_READY {
		st.Phase = klitev1.NodePhase_NODE_PHASE_NOT_READY
		dirty = true
		slog.Info("node lost", "node", name, "silence", silence)
	}
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

// evacuate deletes a dead node's instance objects. The workload controller
// recreates them and the scheduler places them on live nodes. The dead node's
// containers wait for its agent to return and clean up.
func (c *nodeController) evacuate(ctx context.Context, node string, instObjs []*klitev1.Object) error {
	var errs []error
	for _, o := range instObjs {
		inst := o.GetInstance()
		if inst.GetSpec().GetNode() != node {
			continue
		}
		name := inst.GetMeta().GetName()
		if err := c.st.Delete(ctx, object.KindInstance, name); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		slog.Info("instance evacuated from lost node", "instance", name, "node", node)
	}
	return errors.Join(errs...)
}
