package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/runtime"
)

// fakeRuntime is an in-memory Runtime that records every mutating call.
type fakeRuntime struct {
	mu         sync.Mutex
	containers map[string]*runtime.RunningInstance // by container ID
	nextID     int
	runErr     error
	logsFn     func(ctx context.Context, id string, follow bool, tail int32) (io.ReadCloser, error)

	runs     []string // instance names passed to RunInstance
	stops    []string // container IDs
	removes  []string // container IDs
	stopWait []time.Duration

	infra      []runtime.InfraInfo       // ListInfra's canned answer
	hostsFile  []byte                    // ReadContainerFile's canned /etc/hosts
	oneShots   []*runtime.InfraContainer // specs passed to RunOneShot
	oneShotErr error                     // RunOneShot's canned failure
	imageErr   error                     // EnsureImage's canned failure
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{containers: map[string]*runtime.RunningInstance{}}
}

func (f *fakeRuntime) EnsureNetwork(context.Context) error { return nil }

func (f *fakeRuntime) EnsureImage(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageErr
}

func (f *fakeRuntime) RunOneShot(_ context.Context, spec *runtime.InfraContainer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.oneShotErr != nil {
		return f.oneShotErr
	}
	f.oneShots = append(f.oneShots, spec)
	return nil
}

func (f *fakeRuntime) oneShotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.oneShots)
}

func (f *fakeRuntime) RunInfra(context.Context, *runtime.InfraContainer) (string, error) {
	return "", nil
}

func (f *fakeRuntime) InspectInfra(context.Context, string) (*runtime.InfraStatus, error) {
	return nil, nil
}

func (f *fakeRuntime) ListInfra(context.Context, string) ([]runtime.InfraInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.infra), nil
}

func (f *fakeRuntime) ReadContainerFile(context.Context, string, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hostsFile == nil {
		return nil, errors.New("no such container")
	}
	return slices.Clone(f.hostsFile), nil
}

func (f *fakeRuntime) RunInstance(_ context.Context, inst *klitev1.Instance, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return "", f.runErr
	}
	f.nextID++
	id := fmt.Sprintf("ctr-%d", f.nextID)
	f.containers[id] = &runtime.RunningInstance{
		ContainerID:  id,
		InstanceName: inst.GetMeta().GetName(),
		InstanceUID:  inst.GetMeta().GetUid(),
		TemplateHash: inst.GetSpec().GetTemplateHash(),
		State:        runtime.StateRunning,
		IP:           "10.44.128.9",
	}
	f.runs = append(f.runs, inst.GetMeta().GetName())
	return id, nil
}

func (f *fakeRuntime) StopInstance(_ context.Context, id string, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, id)
	f.stopWait = append(f.stopWait, grace)
	if c, ok := f.containers[id]; ok {
		c.State = "exited"
	}
	return nil
}

func (f *fakeRuntime) RemoveInstance(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, id)
	delete(f.containers, id)
	return nil
}

func (f *fakeRuntime) ListInstances(context.Context, string) ([]runtime.RunningInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]runtime.RunningInstance, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b runtime.RunningInstance) int {
		return strings.Compare(a.ContainerID, b.ContainerID)
	})
	return out, nil
}

func (f *fakeRuntime) WatchEvents(context.Context, string) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, nil
}

// Logs delegates to logsFn so command tests can hand out controlled readers.
func (f *fakeRuntime) Logs(ctx context.Context, id string, follow bool, tail int32) (io.ReadCloser, error) {
	f.mu.Lock()
	fn := f.logsFn
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("no logsFn configured")
	}
	return fn(ctx, id, follow, tail)
}

func (f *fakeRuntime) InspectIP(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		return c.IP, nil
	}
	return "", nil
}

// addContainer seeds a pre-existing container, as if left by an earlier agent.
func (f *fakeRuntime) addContainer(ri *runtime.RunningInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *ri
	if cp.ContainerID == "" {
		cp.ContainerID = fmt.Sprintf("ctr-%d", f.nextID)
	}
	f.containers[cp.ContainerID] = &cp
}

// desiredInstance carries a readiness probe, so Running stays Running until a
// probe verdict lands; probeless instances get their own tests.
func desiredInstance(name, uid, hash string) *klitev1.Instance {
	return &klitev1.Instance{
		Meta: &klitev1.Meta{Name: name, Uid: uid},
		Spec: &klitev1.InstanceSpec{
			Workload: "b",
			Node:     "node-1",
			Container: &klitev1.Container{
				Name: "web", Image: "img:1",
				ReadinessProbe: &klitev1.ReadinessProbe{TcpPort: 80},
			},
			TemplateHash: hash,
			Drain:        &klitev1.DrainSpec{TerminationGraceSeconds: 3},
		},
	}
}

func testAgent(t *testing.T, rt runtime.Runtime, instances ...*klitev1.Instance) *Agent {
	t.Helper()
	a := New(&Config{Node: "node-1", Token: "dev-token", Runtime: rt})
	a.applySnapshot(&klitev1.DesiredState{Revision: 1, Instances: instances})
	return a
}

func stateOf(t *testing.T, a *Agent, name string) instState {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.states[name]
	if !ok {
		t.Fatalf("no state recorded for instance %s", name)
	}
	return *st
}

func TestReconcileCreatesMissingInstance(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h1"))

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.runs, []string{"b-aa"}) {
		t.Errorf("runs = %v, want [b-aa]", rt.runs)
	}
	st := stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_RUNNING {
		t.Errorf("phase = %v, want RUNNING", st.phase)
	}
	if st.ip == "" || st.containerID == "" {
		t.Errorf("ip %q and containerID %q must be set", st.ip, st.containerID)
	}
	if st.restarts != 0 {
		t.Errorf("restarts = %d, want 0", st.restarts)
	}
}

func TestReconcileAdoptsRunningContainer(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		InstanceName: "b-aa", InstanceUID: "uid-1", TemplateHash: "h1",
		State: runtime.StateRunning, IP: "10.44.128.4",
	})
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h1"))

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs)+len(rt.stops)+len(rt.removes) != 0 {
		t.Errorf("adoption must not touch Docker: runs=%v stops=%v removes=%v", rt.runs, rt.stops, rt.removes)
	}
	if st := stateOf(t, a, "b-aa"); st.phase != klitev1.InstancePhase_INSTANCE_PHASE_RUNNING || st.ip != "10.44.128.4" {
		t.Errorf("state = %+v, want RUNNING at 10.44.128.4", st)
	}
}

func TestReconcileRemovesOrphans(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-orphan", InstanceName: "b-gone", InstanceUID: "uid-9",
		TemplateHash: "h1", State: runtime.StateRunning,
	})
	a := testAgent(t, rt) // empty snapshot: everything on the node is an orphan

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.stops, []string{"ctr-orphan"}) || !slices.Equal(rt.removes, []string{"ctr-orphan"}) {
		t.Errorf("stops = %v removes = %v, want both [ctr-orphan]", rt.stops, rt.removes)
	}
	if len(rt.runs) != 0 {
		t.Errorf("runs = %v, want none", rt.runs)
	}
}

func TestReconcileRestartsCrashWithBackoff(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-dead", InstanceName: "b-aa", InstanceUID: "uid-1",
		TemplateHash: "h1", State: "exited", ExitCode: 137,
	})
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h1"))
	base := time.Now()
	a.now = func() time.Time { return base }

	// First pass observes the crash and arms the 1s gate.
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", st.phase)
	}
	if st.message != "container exited with code 137" {
		t.Errorf("message = %q", st.message)
	}
	if len(rt.runs) != 0 || len(rt.removes) != 0 {
		t.Fatalf("nothing may happen before the gate opens: runs=%v removes=%v", rt.runs, rt.removes)
	}

	// Half a second in, the gate is still shut.
	a.now = func() time.Time { return base.Add(500 * time.Millisecond) }
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs) != 0 {
		t.Fatal("restarted before the backoff elapsed")
	}

	// Past the window, the agent replaces the container and counts the restart.
	a.now = func() time.Time { return base.Add(1100 * time.Millisecond) }
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.removes, []string{"ctr-dead"}) || !slices.Equal(rt.runs, []string{"b-aa"}) {
		t.Fatalf("removes = %v runs = %v", rt.removes, rt.runs)
	}
	st = stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_RUNNING || st.restarts != 1 {
		t.Errorf("phase = %v restarts = %d, want RUNNING with 1", st.phase, st.restarts)
	}
}

func TestReconcileBackoffDoubles(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h1"))
	now := time.Now()
	a.now = func() time.Time { return now }

	crash := func(id string) {
		rt.addContainer(&runtime.RunningInstance{
			ContainerID: id, InstanceName: "b-aa", InstanceUID: "uid-1",
			TemplateHash: "h1", State: "exited", ExitCode: 1,
		})
	}
	wantBackoffs := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, want := range wantBackoffs {
		rt.mu.Lock()
		clear(rt.containers)
		rt.mu.Unlock()
		crash(fmt.Sprintf("dead-%d", i))
		if err := a.reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(t, a, "b-aa"); st.backoff != want {
			t.Fatalf("crash %d: backoff = %v, want %v", i+1, st.backoff, want)
		}
		now = now.Add(want + 100*time.Millisecond)
		if err := a.reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(t, a, "b-aa"); st.restarts != int32(i+1) {
			t.Fatalf("crash %d: restarts = %d, want %d", i+1, st.restarts, i+1)
		}
	}
}

// M5 note: a hash mismatch used to run a dedicated replaceInstance path. The
// controller owns replacement ordering now, so a mismatched container is an
// orphan (removed first) and the instance starts fresh — same Docker calls,
// different route.
func TestReconcileReplacesOnHashChange(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-old", InstanceName: "b-aa", InstanceUID: "uid-1",
		TemplateHash: "h1", State: runtime.StateRunning,
	})
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h2"))

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.stops, []string{"ctr-old"}) || !slices.Equal(rt.removes, []string{"ctr-old"}) {
		t.Errorf("stops = %v removes = %v, want both [ctr-old]", rt.stops, rt.removes)
	}
	if !slices.Equal(rt.runs, []string{"b-aa"}) {
		t.Errorf("runs = %v, want [b-aa]", rt.runs)
	}
	if !slices.Equal(rt.stopWait, []time.Duration{3 * time.Second}) {
		t.Errorf("stop grace = %v, want the spec's 3s", rt.stopWait)
	}
	st := stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_RUNNING || st.restarts != 0 {
		t.Errorf("phase = %v restarts = %d, want RUNNING with 0", st.phase, st.restarts)
	}
}

func TestReconcileFailedRunArmsRetry(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.runErr = errors.New("no such image")
	a := testAgent(t, rt, desiredInstance("b-aa", "uid-1", "h1"))
	base := time.Now()
	a.now = func() time.Time { return base }

	if err := a.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile must surface the run error")
	}
	st := stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_FAILED || st.message == "" {
		t.Errorf("state = %+v, want FAILED with a message", st)
	}

	// The gate holds even after the error clears, until backoff elapses.
	rt.runErr = nil
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs) != 0 {
		t.Fatal("retried before the backoff elapsed")
	}
	a.now = func() time.Time { return base.Add(1100 * time.Millisecond) }
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.runs, []string{"b-aa"}) {
		t.Errorf("runs = %v, want [b-aa]", rt.runs)
	}
	// A create failure is not a container crash, so nothing was restarted.
	if st := stateOf(t, a, "b-aa"); st.restarts != 0 {
		t.Errorf("restarts = %d, want 0", st.restarts)
	}
}

// drainingInstance is desiredInstance with the controller-set DRAINING phase.
func drainingInstance(name, uid, hash string) *klitev1.Instance {
	inst := desiredInstance(name, uid, hash)
	inst.Status = &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_DRAINING}
	return inst
}

// A draining instance's container keeps serving until the instance leaves
// the snapshot: the agent must not stop, remove, or restart it (ADR 0010).
func TestReconcileDrainingKeepsContainerServing(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-1", InstanceName: "b-aa", InstanceUID: "uid-1",
		TemplateHash: "h1", State: runtime.StateRunning, IP: "10.44.128.7",
	})
	a := testAgent(t, rt, drainingInstance("b-aa", "uid-1", "h1"))

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs)+len(rt.stops)+len(rt.removes) != 0 {
		t.Errorf("draining must not touch Docker: runs=%v stops=%v removes=%v", rt.runs, rt.stops, rt.removes)
	}
	st := stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_DRAINING || st.ip != "10.44.128.7" {
		t.Errorf("state = %+v, want DRAINING with the container's IP", st)
	}
}

// A crash mid-drain is reported, never restarted.
func TestReconcileDrainingCrashReportsWithoutRestart(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-1", InstanceName: "b-aa", InstanceUID: "uid-1",
		TemplateHash: "h1", State: "exited", ExitCode: 137,
	})
	a := testAgent(t, rt, drainingInstance("b-aa", "uid-1", "h1"))
	base := time.Now()
	a.now = func() time.Time { return base }

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := stateOf(t, a, "b-aa")
	if st.phase != klitev1.InstancePhase_INSTANCE_PHASE_FAILED || st.message == "" {
		t.Fatalf("state = %+v, want FAILED with a message", st)
	}
	// Even far past any crash backoff, nothing restarts.
	a.now = func() time.Time { return base.Add(time.Minute) }
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs)+len(rt.removes) != 0 {
		t.Errorf("draining crash must not restart: runs=%v removes=%v", rt.runs, rt.removes)
	}
}

// Once seen draining, an instance stays draining for the agent even when its
// own FAILED report overwrote the store phase in the next snapshot.
func TestReconcileDrainingIsSticky(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-1", InstanceName: "b-aa", InstanceUID: "uid-1",
		TemplateHash: "h1", State: "exited", ExitCode: 1,
	})
	a := testAgent(t, rt, drainingInstance("b-aa", "uid-1", "h1"))
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	failed := desiredInstance("b-aa", "uid-1", "h1")
	failed.Status = &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_FAILED}
	a.applySnapshot(&klitev1.DesiredState{Revision: 2, Instances: []*klitev1.Instance{failed}})
	a.now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs) != 0 {
		t.Errorf("runs = %v, a once-draining instance must never restart", rt.runs)
	}
}

// Deletion mid-drain turns the container into an orphan, stopped with the
// remembered termination grace.
func TestReconcileDrainedInstanceDeletionStopsContainer(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		ContainerID: "ctr-1", InstanceName: "b-aa", InstanceUID: "uid-1",
		TemplateHash: "h1", State: runtime.StateRunning,
	})
	a := testAgent(t, rt, drainingInstance("b-aa", "uid-1", "h1"))
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The controller deleted the instance: next snapshot is empty.
	a.applySnapshot(&klitev1.DesiredState{Revision: 2})
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.stops, []string{"ctr-1"}) || !slices.Equal(rt.removes, []string{"ctr-1"}) {
		t.Errorf("stops = %v removes = %v, want both [ctr-1]", rt.stops, rt.removes)
	}
	if !slices.Equal(rt.stopWait, []time.Duration{3 * time.Second}) {
		t.Errorf("stop grace = %v, want the spec's 3s", rt.stopWait)
	}
}

func TestReconcileSeedsRestartsFromSnapshot(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	rt.addContainer(&runtime.RunningInstance{
		InstanceName: "b-aa", InstanceUID: "uid-1", TemplateHash: "h1",
		State: runtime.StateRunning,
	})
	inst := desiredInstance("b-aa", "uid-1", "h1")
	inst.Status = &klitev1.InstanceStatus{Restarts: 4}
	a := testAgent(t, rt, inst)

	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, a, "b-aa"); st.restarts != 4 {
		t.Errorf("restarts = %d, want the snapshot's 4", st.restarts)
	}
}
