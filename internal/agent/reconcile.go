package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/runtime"
)

func logInstance(msg, name string, args ...any) {
	slog.Info(msg, append([]any{"instance", name}, args...)...)
}

// Crash backoff doubles per consecutive crash: 1s, 2s, 4s ... capped at 30s.
const (
	backoffStart = time.Second
	backoffCap   = 30 * time.Second
)

// instState is what the agent knows about one instance between reconciles.
// Restart counts live here, in memory keyed by instance UID, and persist only
// through status reports (ADR 0011).
type instState struct {
	uid         string
	phase       klitev1.InstancePhase
	restarts    int32
	containerID string
	ip          string
	message     string

	backoff         time.Duration
	nextTry         time.Time
	awaitingRestart bool // a crash was seen and the replacement hasn't started yet
}

func nextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return backoffStart
	}
	return min(d*2, backoffCap)
}

// reconcile converges Docker on the current snapshot. It creates what's
// missing, replaces what changed, restarts what crashed, and removes what
// nobody wants. Orphan cleanup runs on every pass, so it covers agent startup
// for free.
func (a *Agent) reconcile(ctx context.Context) error {
	desired := a.snapshotDesired()
	actual, err := a.rt.ListInstances(ctx, a.node)
	if err != nil {
		return err
	}

	byName := make(map[string]*runtime.RunningInstance, len(actual))
	var orphans []*runtime.RunningInstance
	for i := range actual {
		ri := &actual[i]
		if _, ok := desired[ri.InstanceName]; ok && byName[ri.InstanceName] == nil {
			byName[ri.InstanceName] = ri
		} else {
			orphans = append(orphans, ri)
		}
	}

	var errs []error
	for name, inst := range desired {
		if err := a.reconcileInstance(ctx, inst, byName[name]); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", name, err))
		}
	}
	for _, ri := range orphans {
		if err := a.removeOrphan(ctx, ri); err != nil {
			errs = append(errs, fmt.Errorf("orphan %s: %w", ri.InstanceName, err))
		}
	}
	a.pruneState(desired)
	return errors.Join(errs...)
}

func (a *Agent) reconcileInstance(ctx context.Context, inst *klitev1.Instance, ri *runtime.RunningInstance) error {
	st := a.stateFor(inst)
	switch {
	case ri == nil:
		return a.startInstance(ctx, inst, &st)
	case ri.InstanceUID != inst.GetMeta().GetUid() || ri.TemplateHash != inst.GetSpec().GetTemplateHash():
		return a.replaceInstance(ctx, inst, ri, &st)
	case ri.State == runtime.StateRunning:
		a.markRunning(inst, ri, &st)
		return nil
	default:
		return a.restartInstance(ctx, inst, ri, &st)
	}
}

// startInstance runs the container once the backoff gate is open.
func (a *Agent) startInstance(ctx context.Context, inst *klitev1.Instance, st *instState) error {
	name := inst.GetMeta().GetName()
	if a.now().Before(st.nextTry) {
		a.putState(name, st)
		return nil
	}
	if err := a.rt.EnsureImage(ctx, inst.GetSpec().GetContainer().GetImage()); err != nil {
		return a.failInstance(name, st, err)
	}
	id, err := a.rt.RunInstance(ctx, inst, a.node)
	if err != nil {
		return a.failInstance(name, st, err)
	}
	if st.awaitingRestart {
		st.restarts++
		st.awaitingRestart = false
	}
	// Running doubles as ready until M4 adds readiness probes.
	st.phase = klitev1.InstancePhase_INSTANCE_PHASE_RUNNING
	st.containerID = id
	st.message = ""
	st.ip, _ = a.rt.InspectIP(ctx, id)
	a.putState(name, st)
	logInstance("instance started", name, "restarts", st.restarts)
	return nil
}

// failInstance records the failure and arms the retry gate.
func (a *Agent) failInstance(name string, st *instState, err error) error {
	st.phase = klitev1.InstancePhase_INSTANCE_PHASE_FAILED
	st.message = err.Error()
	st.backoff = nextBackoff(st.backoff)
	st.nextTry = a.now().Add(st.backoff)
	a.putState(name, st)
	return err
}

// restartInstance replaces a crashed container after its backoff. dockerd
// never restarts anything on its own, because the agent owns the loop (ADR 0011).
func (a *Agent) restartInstance(ctx context.Context, inst *klitev1.Instance, ri *runtime.RunningInstance, st *instState) error {
	name := inst.GetMeta().GetName()
	if !st.awaitingRestart {
		st.awaitingRestart = true
		st.backoff = nextBackoff(st.backoff)
		st.nextTry = a.now().Add(st.backoff)
		st.phase = klitev1.InstancePhase_INSTANCE_PHASE_FAILED
		st.message = fmt.Sprintf("container exited with code %d", ri.ExitCode)
		st.containerID = ri.ContainerID
		st.ip = ""
		a.putState(name, st)
		logInstance("instance crashed", name, "exitCode", ri.ExitCode, "retryIn", st.backoff)
		return nil
	}
	if a.now().Before(st.nextTry) {
		a.putState(name, st)
		return nil
	}
	if err := a.rt.RemoveInstance(ctx, ri.ContainerID); err != nil {
		return a.failInstance(name, st, err)
	}
	return a.startInstance(ctx, inst, st)
}

// replaceInstance swaps a container whose template hash or instance UID no
// longer matches the spec. Stop-then-start is the M2 shape. Surge-first drain
// choreography lands in M6 (ADR 0010).
func (a *Agent) replaceInstance(ctx context.Context, inst *klitev1.Instance, ri *runtime.RunningInstance, st *instState) error {
	name := inst.GetMeta().GetName()
	st.phase = klitev1.InstancePhase_INSTANCE_PHASE_TERMINATING
	st.message = "replacing: template changed"
	a.putState(name, st)
	logInstance("replacing instance container", name, "hash", inst.GetSpec().GetTemplateHash())
	if err := a.rt.StopInstance(ctx, ri.ContainerID, graceOf(inst)); err != nil {
		return a.failInstance(name, st, err)
	}
	if err := a.rt.RemoveInstance(ctx, ri.ContainerID); err != nil {
		return a.failInstance(name, st, err)
	}
	st.phase = klitev1.InstancePhase_INSTANCE_PHASE_PENDING
	st.containerID = ""
	st.ip = ""
	st.message = ""
	return a.startInstance(ctx, inst, st)
}

// markRunning adopts a live container, including ones inherited from a
// previous agent life.
func (a *Agent) markRunning(inst *klitev1.Instance, ri *runtime.RunningInstance, st *instState) {
	st.awaitingRestart = false
	// Running doubles as ready until M4 adds readiness probes.
	st.phase = klitev1.InstancePhase_INSTANCE_PHASE_RUNNING
	st.containerID = ri.ContainerID
	if ri.IP != "" {
		st.ip = ri.IP
	}
	st.message = ""
	a.putState(inst.GetMeta().GetName(), st)
}

// removeOrphan stops and removes a container whose instance is no longer
// desired on this node.
func (a *Agent) removeOrphan(ctx context.Context, ri *runtime.RunningInstance) error {
	logInstance("removing orphan container", ri.InstanceName, "container", short(ri.ContainerID))
	if ri.State == runtime.StateRunning {
		if err := a.rt.StopInstance(ctx, ri.ContainerID, a.rememberedGrace(ri.InstanceName)); err != nil {
			return err
		}
	}
	if err := a.rt.RemoveInstance(ctx, ri.ContainerID); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.grace, ri.InstanceName)
	a.mu.Unlock()
	return nil
}

// stateFor returns a working copy of the instance's state. A UID the agent
// hasn't seen gets its restart count seeded from the snapshot's status, so an
// agent restart doesn't zero the RESTARTS column.
func (a *Agent) stateFor(inst *klitev1.Instance) instState {
	name := inst.GetMeta().GetName()
	uid := inst.GetMeta().GetUid()
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.states[name]; ok && st.uid == uid {
		return *st
	}
	return instState{
		uid:      uid,
		phase:    klitev1.InstancePhase_INSTANCE_PHASE_PENDING,
		restarts: inst.GetStatus().GetRestarts(),
	}
}

// putState commits a working copy and requests an immediate status report
// when anything reportable changed.
func (a *Agent) putState(name string, st *instState) {
	a.mu.Lock()
	prev, ok := a.states[name]
	changed := !ok || prev.phase != st.phase || prev.restarts != st.restarts ||
		prev.containerID != st.containerID || prev.ip != st.ip || prev.message != st.message
	cp := *st
	a.states[name] = &cp
	a.mu.Unlock()
	if changed {
		kick(a.kickReport)
	}
}

// pruneState drops bookkeeping for instances that left the snapshot.
func (a *Agent) pruneState(desired map[string]*klitev1.Instance) {
	a.mu.Lock()
	defer a.mu.Unlock()
	maps.DeleteFunc(a.states, func(name string, _ *instState) bool {
		_, ok := desired[name]
		return !ok
	})
}

func (a *Agent) snapshotDesired() map[string]*klitev1.Instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.desired)
}

// rememberedGrace returns the stop grace last seen for an instance name.
// Orphans inherited by a fresh agent have no snapshot to consult, so those
// fall back to the default.
func (a *Agent) rememberedGrace(name string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if g, ok := a.grace[name]; ok {
		return time.Duration(g) * time.Second
	}
	return object.DefaultTerminationGraceSeconds * time.Second
}

func graceOf(inst *klitev1.Instance) time.Duration {
	if g := inst.GetSpec().GetDrain().GetTerminationGraceSeconds(); g > 0 {
		return time.Duration(g) * time.Second
	}
	return object.DefaultTerminationGraceSeconds * time.Second
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
