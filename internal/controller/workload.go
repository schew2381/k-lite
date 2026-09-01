package controller

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// workloadController materializes instance objects from workload specs, keeps
// workload status counts current, and runs the surge-first drain choreography
// (ADR 0010) in rollout.go.
type workloadController struct {
	st  store.Store
	now func() time.Time
	// deadlines maps instance UID to the moment its drain expires and the
	// instance may be deleted. In-memory on purpose: a fresh leader that
	// finds an unknown DRAINING instance restarts its clock, which only
	// lengthens the drain (rollout.go).
	deadlines map[string]time.Time
}

func newWorkloadController(st store.Store) *workloadController {
	return &workloadController{st: st, now: time.Now, deadlines: map[string]time.Time{}}
}

func (c *workloadController) reconcile(ctx context.Context) error {
	wls, _, err := c.st.List(ctx, object.KindWorkload)
	if err != nil {
		return err
	}
	insts, _, err := c.st.List(ctx, object.KindInstance)
	if err != nil {
		return err
	}
	drainingNodes, err := c.drainingNodes(ctx)
	if err != nil {
		return err
	}
	byWorkload := make(map[string][]*klitev1.Instance)
	uids := make(map[string]bool, len(insts))
	for _, o := range insts {
		inst := o.GetInstance()
		w := inst.GetSpec().GetWorkload()
		byWorkload[w] = append(byWorkload[w], inst)
		uids[inst.GetMeta().GetUid()] = true
	}
	maps.DeleteFunc(c.deadlines, func(uid string, _ time.Time) bool { return !uids[uid] })

	var errs []error
	live := make(map[string]bool, len(wls))
	for _, o := range wls {
		name := o.GetWorkload().GetMeta().GetName()
		live[name] = true
		errs = append(errs, c.reconcileWorkload(ctx, o, byWorkload[name], drainingNodes))
	}
	// Instances whose workload is gone get deleted, and the owning agents
	// then drop the containers.
	for wname, list := range byWorkload {
		if live[wname] {
			continue
		}
		for _, inst := range list {
			errs = append(errs, c.deleteInstance(ctx, inst))
		}
	}
	return errors.Join(errs...)
}

// drainingNodes names the nodes mid-drain: their instances count as retiring
// so the rollout machinery replaces them elsewhere (ADR 0010).
func (c *workloadController) drainingNodes(ctx context.Context) (map[string]bool, error) {
	nodeObjs, _, err := c.st.List(ctx, object.KindNode)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range nodeObjs {
		if o.GetNode().GetStatus().GetPhase() == klitev1.NodePhase_NODE_PHASE_DRAINING {
			out[o.GetNode().GetMeta().GetName()] = true
		}
	}
	return out, nil
}

func (c *workloadController) reconcileWorkload(ctx context.Context, obj *klitev1.Object, instances []*klitev1.Instance, drainingNodes map[string]bool) error {
	w := obj.GetWorkload()
	name := w.GetMeta().GetName()
	if len(w.GetSpec().GetTemplate().GetContainers()) != 1 {
		return fmt.Errorf("workload %s: template must hold exactly one container", name)
	}
	hash, err := object.TemplateHash(w.GetSpec().GetTemplate())
	if err != nil {
		return fmt.Errorf("workload %s: %w", name, err)
	}
	return errors.Join(
		c.advance(ctx, w, hash, instances, drainingNodes),
		c.updateStatus(ctx, obj, hash, instances),
	)
}

// sortNewestFirst orders by creation time, then store revision, then name so
// same-second creations still order deterministically.
func sortNewestFirst(instances []*klitev1.Instance) {
	slices.SortFunc(instances, func(x, y *klitev1.Instance) int {
		if d := cmp.Compare(y.GetMeta().GetCreatedUnix(), x.GetMeta().GetCreatedUnix()); d != 0 {
			return d
		}
		if d := cmp.Compare(y.GetMeta().GetResourceVersion(), x.GetMeta().GetResourceVersion()); d != 0 {
			return d
		}
		return cmp.Compare(y.GetMeta().GetName(), x.GetMeta().GetName())
	})
}

// createInstance materializes one instance for the workload. A name collision
// on the random suffix rolls again.
func (c *workloadController) createInstance(ctx context.Context, w *klitev1.Workload, hash string) error {
	spec := w.GetSpec()
	wname := w.GetMeta().GetName()
	for range casRetries {
		name := wname + "-" + hexSuffix()
		inst := &klitev1.Instance{
			Meta: &klitev1.Meta{Name: name, Labels: maps.Clone(spec.GetTemplate().GetLabels())},
			Spec: &klitev1.InstanceSpec{
				Workload:     wname,
				Container:    proto.CloneOf(spec.GetTemplate().GetContainers()[0]),
				TemplateHash: hash,
				Drain:        proto.CloneOf(spec.GetDrain()),
			},
			Status: &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_PENDING},
		}
		_, err := c.st.Put(ctx, &klitev1.Object{Kind: &klitev1.Object_Instance{Instance: inst}}, store.RevCreate)
		if err == nil {
			slog.Info("instance created", "workload", wname, "instance", name)
			return nil
		}
		if !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
	}
	return fmt.Errorf("workload %s: kept colliding on instance names", wname)
}

func (c *workloadController) deleteInstance(ctx context.Context, inst *klitev1.Instance) error {
	name := inst.GetMeta().GetName()
	err := c.st.Delete(ctx, object.KindInstance, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil // another leader life got here first
	case err != nil:
		return err
	}
	slog.Info("instance deleted", "workload", inst.GetSpec().GetWorkload(), "instance", name)
	return nil
}

// updateStatus writes observed counts. Ready counts every READY instance
// regardless of template hash: old-template instances keep serving through a
// rollout, and a surge may push both counts past replicas briefly.
func (c *workloadController) updateStatus(ctx context.Context, obj *klitev1.Object, hash string, instances []*klitev1.Instance) error {
	w := obj.GetWorkload()
	var ready int32
	for _, inst := range instances {
		if inst.GetStatus().GetPhase() == klitev1.InstancePhase_INSTANCE_PHASE_READY {
			ready++
		}
	}
	next := &klitev1.WorkloadStatus{
		ReadyInstances: ready,
		TotalInstances: int32(len(instances)),
		TemplateHash:   hash,
	}
	if proto.Equal(w.GetStatus(), next) {
		return nil
	}
	w.Status = next
	// A lost revision race resolves on the next pass.
	if _, err := c.st.Put(ctx, obj, w.GetMeta().GetResourceVersion()); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}

func hexSuffix() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}
