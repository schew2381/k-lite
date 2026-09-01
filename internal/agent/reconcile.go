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
	draining        bool // sticky: once seen DRAINING, never restarted (ADR 0010)
}

func nextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return backoffStart
	}
	return min(d*2, backoffCap)
}

// reconcile converges Docker on the current snapshot. It creates what's
// missing, restarts what crashed, and removes what nobody wants. Orphan
// cleanup runs first — a container whose UID or hash no longer matches its
// instance has lost its claim to the name — and on every pass, so it covers
// agent startup for free. The controller owns replacement ordering (ADR
// 0010); the agent only converges on the instance list it's told.
func (a *Agent) reconcile(ctx context.Context) error {
	desired := a.snapshotDesired()
	actual, err := a.rt.ListInstances(ctx, a.node)
	if err != nil {
		return err
	}
	byName, orphans := matchContainers(desired, actual)

	var errs []error
	for _, ri := range orphans {
		if err := a.removeOrphan(ctx, ri); err != nil {
			errs = append(errs, fmt.Errorf("orphan %s: %w", ri.InstanceName, err))
		}
	}
	for name, inst := range desired {
		if err := a.reconcileInstance(ctx, inst, byName[name]); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", name, err))
		}
	}
	a.pruneState(desired)
	return errors.Join(errs...)
}

// matchContainers pairs containers with the instances they belong to. A
// container only counts as an instance's when name, UID, and template hash
// all agree; anything else is an orphan.
func matchContainers(desired map[string]*klitev1.Instance, actual []runtime.RunningInstance) (map[string]*runtime.RunningInstance, []*runtime.RunningInstance) {
	byName := make(map[string]*runtime.RunningInstance, len(actual))
	var orphans []*runtime.RunningInstance
	for i := range actual {
		ri := &actual[i]
		inst, ok := desired[ri.InstanceName]
		if ok && byName[ri.InstanceName] == nil &&
			ri.InstanceUID == inst.GetMeta().GetUid() &&
			ri.TemplateHash == inst.GetSpec().GetTemplateHash() {
			byName[ri.InstanceName] = ri
		} else {
			orphans = append(orphans, ri)
		}
	}
	return byName, orphans
}

func (a *Agent) reconcileInstance(ctx context.Context, inst *klitev1.Instance, ri *runtime.RunningInstance) error {
	st := a.stateFor(inst)
	st.draining = st.draining || inst.GetStatus().GetPhase() == klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
	if st.draining {
		a.observeDraining(inst, ri, &st)
		return nil
	}
	switch {
	case ri == nil:
		return a.startInstance(ctx, inst, &st)
	case ri.State == runtime.StateRunning:
		a.markRunning(inst, ri, &st)
		return nil
	default:
		return a.restartInstance(ctx, inst, ri, &st)
	}
}

// observeDraining reports on a draining instance without touching its
// container: it keeps serving in-flight connections until the controller
// deletes the instance, and Envoy stops sending it new ones via EDS. A crash
// mid-drain is reported, never restarted (ADR 0010).
func (a *Agent) observeDraining(inst *klitev1.Instance, ri *runtime.RunningInstance, st *instState) {
	switch {
	case ri == nil:
		st.phase = klitev1.InstancePhase_INSTANCE_PHASE_FAILED
		st.message = "container gone while draining"
		st.containerID = ""
		st.ip = ""
	case ri.State == runtime.StateRunning:
		st.phase = klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
		st.containerID = ri.ContainerID
		if ri.IP != "" {
			st.ip = ri.IP
		}
		st.message = ""
	default:
		st.phase = klitev1.InstancePhase_INSTANCE_PHASE_FAILED
		st.message = fmt.Sprintf("container exited with code %d while draining", ri.ExitCode)
		st.containerID = ri.ContainerID
		st.ip = ""
	}
	a.putState(inst.GetMeta().GetName(), st)
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
	id, err := a.rt.RunInstance(ctx, inst, a.node, a.dnsIP())
	if err != nil {
		return a.failInstance(name, st, err)
	}
	if st.awaitingRestart {
		st.restarts++
		st.awaitingRestart = false
	}
	st.phase = a.runningPhase(inst)
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

// markRunning adopts a live container, including ones inherited from a
// previous agent life.
func (a *Agent) markRunning(inst *klitev1.Instance, ri *runtime.RunningInstance, st *instState) {
	st.awaitingRestart = false
	st.phase = a.runningPhase(inst)
	st.containerID = ri.ContainerID
	if ri.IP != "" {
		st.ip = ri.IP
	}
	st.message = ""
	a.putState(inst.GetMeta().GetName(), st)
}

// runningPhase decides what "the container runs" means for this instance:
// READY outright without a readiness probe, otherwise READY only once
// klite-net's TCP probe agrees (ADR 0008).
func (a *Agent) runningPhase(inst *klitev1.Instance) klitev1.InstancePhase {
	if inst.GetSpec().GetContainer().GetReadinessProbe().GetTcpPort() <= 0 {
		return klitev1.InstancePhase_INSTANCE_PHASE_READY
	}
	a.mu.Lock()
	ready := a.probeReady[inst.GetMeta().GetName()]
	a.mu.Unlock()
	if ready {
		return klitev1.InstancePhase_INSTANCE_PHASE_READY
	}
	return klitev1.InstancePhase_INSTANCE_PHASE_RUNNING
}

// dnsIP is the node's klite-net address, every workload container's one
// resolver.
func (a *Agent) dnsIP() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.net.GetKliteNetIp()
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
	// A stale container of a still-desired instance keeps its grace entry
	// for the fresh container that follows.
	if _, still := a.desired[ri.InstanceName]; !still {
		delete(a.grace, ri.InstanceName)
	}
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

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
