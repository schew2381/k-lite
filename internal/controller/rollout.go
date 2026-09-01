package controller

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
)

// groups splits one workload's instances by their role in the surge-first
// choreography (ADR 0010).
type groups struct {
	// fresh instances carry the current template hash on a node that isn't
	// draining. They are, or will become, the serving set.
	fresh []*klitev1.Instance
	// retiring instances are READY but on a stale hash or a draining node.
	// They keep serving until a replacement is READY, then drain out.
	retiring []*klitev1.Instance
	// doomed instances are retiring but not READY: never in EDS, so
	// deleting them outright dips nothing.
	doomed []*klitev1.Instance
	// draining instances are waiting out their drain timeout.
	draining []*klitev1.Instance
}

func classify(hash string, instances []*klitev1.Instance, drainingNodes map[string]bool) groups {
	var g groups
	for _, inst := range instances {
		switch phase := inst.GetStatus().GetPhase(); {
		case phase == klitev1.InstancePhase_INSTANCE_PHASE_DRAINING:
			g.draining = append(g.draining, inst)
		case inst.GetSpec().GetTemplateHash() == hash && !drainingNodes[inst.GetSpec().GetNode()]:
			g.fresh = append(g.fresh, inst)
		case phase == klitev1.InstancePhase_INSTANCE_PHASE_READY:
			g.retiring = append(g.retiring, inst)
		default:
			g.doomed = append(g.doomed, inst)
		}
	}
	sortNewestFirst(g.fresh)
	sortNewestFirst(g.retiring)
	return g
}

// advance moves one workload toward `replicas` instances of `hash` without
// dipping below the replica count: create one surge instance, wait for READY,
// drain one retiring instance, delete it after its drain timeout, repeat.
// Every step is level-based, so a rerun after failover picks up mid-dance.
func (c *workloadController) advance(ctx context.Context, w *klitev1.Workload, hash string, instances []*klitev1.Instance, drainingNodes map[string]bool) error {
	g := classify(hash, instances, drainingNodes)
	replicas := int(w.GetSpec().GetReplicas())
	var errs []error
	for _, inst := range g.doomed {
		errs = append(errs, c.deleteInstance(ctx, inst))
	}
	errs = append(errs, c.expireDrains(ctx, g.draining))

	// On scale-down, surplus fresh instances drain out newest-first, all
	// at once, since the survivors already cover the replica count.
	if surplus := len(g.fresh) - replicas; surplus > 0 {
		for _, inst := range g.fresh[:surplus] {
			errs = append(errs, c.retire(ctx, inst, "scale-down"))
		}
		return errors.Join(errs...)
	}
	// Base capacity is restored in bulk because initial create, scale-up,
	// and refill after a node evacuation are not rolling updates.
	if missing := replicas - len(g.fresh) - len(g.retiring); missing > 0 {
		for range missing {
			errs = append(errs, c.createInstance(ctx, w, hash))
		}
		return errors.Join(errs...)
	}
	// Only one retirement is in flight at a time, so nothing new starts
	// while an instance is draining.
	if len(g.retiring) == 0 || len(g.draining) > 0 {
		return errors.Join(errs...)
	}
	errs = append(errs, c.rollStep(ctx, w, hash, replicas, &g))
	return errors.Join(errs...)
}

// rollStep performs at most one rollout action: surge, or mark the next
// victim DRAINING once enough fresh instances are READY. A surge that can't
// schedule falls back to drain-first for that one instance (ADR 0010).
func (c *workloadController) rollStep(ctx context.Context, w *klitev1.Workload, hash string, replicas int, g *groups) error {
	ready := 0
	for _, inst := range g.fresh {
		if inst.GetStatus().GetPhase() == klitev1.InstancePhase_INSTANCE_PHASE_READY {
			ready++
		}
	}
	switch {
	case len(g.fresh)+len(g.retiring) == replicas && ready == len(g.fresh):
		return c.createInstance(ctx, w, hash) // the surge
	case len(g.fresh)+len(g.retiring) > replicas && ready >= min(replicas, len(g.fresh)):
		return c.retire(ctx, g.retiring[0], "rollout")
	case len(g.fresh)+len(g.retiring) > replicas && surgeBlocked(g.fresh):
		slog.Warn("no capacity to surge, falling back to drain-first",
			"workload", w.GetMeta().GetName(), "instance", g.retiring[0].GetMeta().GetName())
		return c.retire(ctx, g.retiring[0], "drain-first fallback: no capacity to surge")
	}
	return nil
}

// surgeBlocked reports whether an unbound fresh instance sits behind a
// placement the scheduler already refused, meaning waiting for the surge
// would wait forever.
func surgeBlocked(fresh []*klitev1.Instance) bool {
	for _, inst := range fresh {
		if inst.GetSpec().GetNode() != "" {
			continue
		}
		msg := inst.GetStatus().GetMessage()
		if msg == MsgNoCapacity || strings.HasPrefix(msg, "pinned node ") {
			return true
		}
	}
	return false
}

// retire moves one instance out of service. A READY instance goes DRAINING
// so Envoy stops handing it new connections while in-flight ones finish.
// Anything else was never in EDS and is deleted outright.
func (c *workloadController) retire(ctx context.Context, inst *klitev1.Instance, reason string) error {
	if inst.GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_READY {
		return c.deleteInstance(ctx, inst)
	}
	inst.Status.Phase = klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
	inst.Status.Message = reason
	obj := &klitev1.Object{Kind: &klitev1.Object_Instance{Instance: inst}}
	if _, err := c.st.Put(ctx, obj, inst.GetMeta().GetResourceVersion()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil // the next pass re-reads and retries
		}
		return err
	}
	timeout := drainTimeout(inst)
	c.deadlines[inst.GetMeta().GetUid()] = c.now().Add(timeout)
	slog.Info("instance draining", "instance", inst.GetMeta().GetName(), "timeout", timeout, "reason", reason)
	return nil
}

// expireDrains deletes draining instances whose deadline passed. An unknown
// deadline (fresh leadership) restarts the clock rather than cutting the
// drain short.
func (c *workloadController) expireDrains(ctx context.Context, draining []*klitev1.Instance) error {
	var errs []error
	for _, inst := range draining {
		uid := inst.GetMeta().GetUid()
		deadline, ok := c.deadlines[uid]
		if !ok {
			c.deadlines[uid] = c.now().Add(drainTimeout(inst))
			continue
		}
		if !c.now().Before(deadline) {
			errs = append(errs, c.deleteInstance(ctx, inst))
		}
	}
	return errors.Join(errs...)
}

func drainTimeout(inst *klitev1.Instance) time.Duration {
	if s := inst.GetSpec().GetDrain().GetDrainTimeoutSeconds(); s > 0 {
		return time.Duration(s) * time.Second
	}
	return object.DefaultDrainTimeoutSeconds * time.Second
}
