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

	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// workloadController materializes instance objects from workload specs and
// keeps workload status counts current.
type workloadController struct {
	st store.Store
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
	byWorkload := make(map[string][]*klitev1.Instance)
	for _, o := range insts {
		inst := o.GetInstance()
		w := inst.GetSpec().GetWorkload()
		byWorkload[w] = append(byWorkload[w], inst)
	}

	var errs []error
	live := make(map[string]bool, len(wls))
	for _, o := range wls {
		name := o.GetWorkload().GetMeta().GetName()
		live[name] = true
		errs = append(errs, c.reconcileWorkload(ctx, o, byWorkload[name]))
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

func (c *workloadController) reconcileWorkload(ctx context.Context, obj *klitev1.Object, instances []*klitev1.Instance) error {
	w := obj.GetWorkload()
	name := w.GetMeta().GetName()
	if len(w.GetSpec().GetTemplate().GetContainers()) != 1 {
		return fmt.Errorf("workload %s: template must hold exactly one container", name)
	}
	hash, err := object.TemplateHash(w.GetSpec().GetTemplate())
	if err != nil {
		return fmt.Errorf("workload %s: %w", name, err)
	}
	create, del := diffInstances(int(w.GetSpec().GetReplicas()), hash, instances)
	var errs []error
	for _, inst := range del {
		errs = append(errs, c.deleteInstance(ctx, inst))
	}
	for range create {
		errs = append(errs, c.createInstance(ctx, w, hash))
	}
	errs = append(errs, c.updateStatus(ctx, obj, hash, instances))
	return errors.Join(errs...)
}

// diffInstances decides creations and deletions for one workload. Instances on
// a stale template hash all go at once and come back on the new hash. M5
// replaces that with the surge-first rolling update (ADR 0010). Scale-down
// removes newest-first.
func diffInstances(replicas int, hash string, instances []*klitev1.Instance) (create int, del []*klitev1.Instance) {
	var current []*klitev1.Instance
	for _, inst := range instances {
		if inst.GetSpec().GetTemplateHash() == hash {
			current = append(current, inst)
		} else {
			del = append(del, inst)
		}
	}
	slices.SortFunc(current, func(x, y *klitev1.Instance) int {
		// Newest first: creation time, then store revision, then name so
		// same-second creations still order deterministically.
		if d := cmp.Compare(y.GetMeta().GetCreatedUnix(), x.GetMeta().GetCreatedUnix()); d != 0 {
			return d
		}
		if d := cmp.Compare(y.GetMeta().GetResourceVersion(), x.GetMeta().GetResourceVersion()); d != 0 {
			return d
		}
		return cmp.Compare(y.GetMeta().GetName(), x.GetMeta().GetName())
	})
	if len(current) > replicas {
		del = append(del, current[:len(current)-replicas]...)
	}
	return max(0, replicas-len(current)), del
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
	if err := c.st.Delete(ctx, object.KindInstance, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	slog.Info("instance deleted", "workload", inst.GetSpec().GetWorkload(), "instance", name)
	return nil
}

// updateStatus writes observed counts. Running counts as ready until M4 wires
// readiness probes.
func (c *workloadController) updateStatus(ctx context.Context, obj *klitev1.Object, hash string, instances []*klitev1.Instance) error {
	w := obj.GetWorkload()
	var ready int32
	for _, inst := range instances {
		if inst.GetSpec().GetTemplateHash() != hash {
			continue
		}
		switch inst.GetStatus().GetPhase() {
		case klitev1.InstancePhase_INSTANCE_PHASE_RUNNING, klitev1.InstancePhase_INSTANCE_PHASE_READY:
			ready++
		default:
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
