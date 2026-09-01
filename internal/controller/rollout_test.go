package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/store"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

// workloadObj builds a valid workload whose template hash varies with ver.
func workloadObj(name string, replicas int32, ver string, drainSecs int32) *klitev1.Object {
	return &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.WorkloadSpec{
			Replicas: replicas,
			Template: &klitev1.Template{
				Labels: map[string]string{"app": name},
				Containers: []*klitev1.Container{{
					Name: "web", Image: "img:1",
					Env: []*klitev1.EnvVar{{Name: "VER", Value: ver}},
				}},
			},
			Drain: &klitev1.DrainSpec{DrainTimeoutSeconds: drainSecs, TerminationGraceSeconds: drainSecs},
		},
	}}}
}

// harness drives the workload controller against the in-memory store with a
// hand-cranked clock.
type harness struct {
	t   *testing.T
	st  *storetest.Memory
	c   *workloadController
	now time.Time
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, st: storetest.New(), now: time.Unix(1000, 0), ctx: context.Background()}
	h.c = newWorkloadController(h.st)
	h.c.now = func() time.Time { return h.now }
	return h
}

func (h *harness) put(obj *klitev1.Object) {
	h.t.Helper()
	if _, err := h.st.Put(h.ctx, obj, store.RevAny); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) seedInstance(workload, name, hash, node string, phase klitev1.InstancePhase) {
	h.put(&klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
		Meta: &klitev1.Meta{Name: name, Labels: map[string]string{"app": workload}},
		Spec: &klitev1.InstanceSpec{
			Workload: workload, Node: node, TemplateHash: hash,
			Container: &klitev1.Container{Name: "web", Image: "img:0"},
			Drain:     &klitev1.DrainSpec{DrainTimeoutSeconds: 5, TerminationGraceSeconds: 5},
		},
		Status: &klitev1.InstanceStatus{Phase: phase},
	}}})
}

func (h *harness) pass() {
	h.t.Helper()
	if err := h.c.reconcile(h.ctx); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

func (h *harness) instances() []*klitev1.Instance {
	h.t.Helper()
	objs, _, err := h.st.List(h.ctx, object.KindInstance)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]*klitev1.Instance, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetInstance())
	}
	return out
}

// count tallies instances matching hash (or any hash for "") and phase.
func (h *harness) count(hash string, phase klitev1.InstancePhase) int {
	n := 0
	for _, inst := range h.instances() {
		if hash != "" && inst.GetSpec().GetTemplateHash() != hash {
			continue
		}
		if inst.GetStatus().GetPhase() == phase {
			n++
		}
	}
	return n
}

func (h *harness) setPhase(name string, phase klitev1.InstancePhase) {
	h.t.Helper()
	obj, rev, err := h.st.Get(h.ctx, object.KindInstance, name)
	if err != nil {
		h.t.Fatal(err)
	}
	obj.GetInstance().Status.Phase = phase
	obj.GetInstance().Status.Message = ""
	if _, err := h.st.Put(h.ctx, obj, rev); err != nil {
		h.t.Fatal(err)
	}
}

// firstWhere returns the first instance matching pred, or nil.
func (h *harness) firstWhere(pred func(*klitev1.Instance) bool) *klitev1.Instance {
	for _, inst := range h.instances() {
		if pred(inst) {
			return inst
		}
	}
	return nil
}

func hashOf(t *testing.T, obj *klitev1.Object) string {
	t.Helper()
	hash, err := object.TemplateHash(obj.GetWorkload().GetSpec().GetTemplate())
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// TestRolloutOneAtATime walks a 2-replica template change through the full
// surge → ready → drain → delete dance and asserts strict sequencing: never
// more than one surge or one draining instance, never fewer serving
// instances than replicas.
func TestRolloutOneAtATime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 2, "v2", 5)
	newHash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-old1", "h-old", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-old2", "h-old", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	assertCounts := func(step string, total, fresh, draining int) {
		t.Helper()
		if got := len(h.instances()); got != total {
			t.Fatalf("%s: total = %d, want %d", step, got, total)
		}
		freshGot := 0
		for _, inst := range h.instances() {
			if inst.GetSpec().GetTemplateHash() == newHash {
				freshGot++
			}
		}
		if freshGot != fresh {
			t.Fatalf("%s: fresh = %d, want %d", step, freshGot, fresh)
		}
		if got := h.count("", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING); got != draining {
			t.Fatalf("%s: draining = %d, want %d", step, got, draining)
		}
		serving := h.count("", klitev1.InstancePhase_INSTANCE_PHASE_READY) +
			h.count("", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING)
		if serving < 2 {
			t.Fatalf("%s: serving = %d, dipped below replicas", step, serving)
		}
	}

	h.pass()
	assertCounts("surge created", 3, 1, 0)
	h.pass()
	assertCounts("no double surge while pending", 3, 1, 0)

	surge := h.firstWhere(func(i *klitev1.Instance) bool { return i.GetSpec().GetTemplateHash() == newHash })
	h.setPhase(surge.GetMeta().GetName(), klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.pass()
	assertCounts("newest old draining after surge ready", 3, 1, 1)
	victim := h.firstWhere(func(i *klitev1.Instance) bool {
		return i.GetStatus().GetPhase() == klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
	})
	if victim.GetMeta().GetName() != "b-old2" {
		t.Fatalf("draining %s, want the newest old instance b-old2", victim.GetMeta().GetName())
	}

	h.now = h.now.Add(2 * time.Second)
	h.pass()
	assertCounts("drain timeout not yet reached", 3, 1, 1)

	h.now = h.now.Add(4 * time.Second) // past the 5s timeout
	h.pass()
	assertCounts("first old deleted", 2, 1, 0)

	h.pass()
	assertCounts("second surge", 3, 2, 0)
	for _, inst := range h.instances() {
		if inst.GetSpec().GetTemplateHash() == newHash && inst.GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_READY {
			h.setPhase(inst.GetMeta().GetName(), klitev1.InstancePhase_INSTANCE_PHASE_READY)
		}
	}
	h.pass()
	assertCounts("second old draining", 3, 2, 1)
	h.now = h.now.Add(6 * time.Second)
	h.pass()
	assertCounts("rollout complete", 2, 2, 0)
	h.pass()
	assertCounts("stable", 2, 2, 0)
}

// Instances that never served (stale hash, not READY) are replaced in bulk:
// no drain buys anything for an endpoint that was never in EDS.
func TestNeverReadyStaleReplacedInBulk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 2, "v2", 5)
	h.put(wl)
	h.seedInstance("b", "b-old1", "h-old", "", klitev1.InstancePhase_INSTANCE_PHASE_PENDING)
	h.seedInstance("b", "b-old2", "h-old", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_RUNNING)

	h.pass()
	insts := h.instances()
	if len(insts) != 2 {
		t.Fatalf("instances = %d, want 2", len(insts))
	}
	newHash := hashOf(t, wl)
	for _, inst := range insts {
		if inst.GetSpec().GetTemplateHash() != newHash {
			t.Errorf("instance %s kept stale hash", inst.GetMeta().GetName())
		}
	}
}

func TestScaleDownDrainsNewestFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v1", 5)
	hash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-aa", hash, "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-bb", hash, "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-cc", hash, "node-3", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	h.pass()
	if got := h.count("", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING); got != 2 {
		t.Fatalf("draining = %d, want both surplus instances", got)
	}
	if oldest, _, err := h.st.Get(h.ctx, object.KindInstance, "b-aa"); err != nil ||
		oldest.GetInstance().GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_READY {
		t.Fatalf("b-aa must stay READY, got %v (%v)", oldest.GetInstance().GetStatus().GetPhase(), err)
	}
	// Victims are gone only after the drain timeout.
	h.pass()
	if got := len(h.instances()); got != 3 {
		t.Fatalf("instances = %d, want 3 while draining", got)
	}
	h.now = h.now.Add(6 * time.Second)
	h.pass()
	insts := h.instances()
	if len(insts) != 1 || insts[0].GetMeta().GetName() != "b-aa" {
		t.Fatalf("instances = %v, want only b-aa", names(insts))
	}
}

// A surplus instance that is not READY is deleted outright, no drain.
func TestScaleDownDeletesNonReadyImmediately(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v1", 5)
	hash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-aa", hash, "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-bb", hash, "node-2", klitev1.InstancePhase_INSTANCE_PHASE_RUNNING)

	h.pass()
	insts := h.instances()
	if len(insts) != 1 || insts[0].GetMeta().GetName() != "b-aa" {
		t.Fatalf("instances = %v, want only b-aa", names(insts))
	}
}

// When the surge can't schedule anywhere, the old instance drains first
// (the documented dip), freeing its slot.
func TestRolloutCapacityFallback(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v2", 5)
	newHash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-old", "h-old", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	h.pass() // surge created, unbound
	surge := h.firstWhere(func(i *klitev1.Instance) bool { return i.GetSpec().GetTemplateHash() == newHash })
	if surge == nil {
		t.Fatal("no surge instance created")
	}
	// The scheduler found no room.
	obj, rev, err := h.st.Get(h.ctx, object.KindInstance, surge.GetMeta().GetName())
	if err != nil {
		t.Fatal(err)
	}
	obj.GetInstance().Status.Message = MsgNoCapacity
	if _, err := h.st.Put(h.ctx, obj, rev); err != nil {
		t.Fatal(err)
	}

	h.pass()
	old, _, err := h.st.Get(h.ctx, object.KindInstance, "b-old")
	if err != nil {
		t.Fatal(err)
	}
	if old.GetInstance().GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_DRAINING {
		t.Fatalf("old phase = %v, want DRAINING via fallback", old.GetInstance().GetStatus().GetPhase())
	}
	if !strings.Contains(old.GetInstance().GetStatus().GetMessage(), "fallback") {
		t.Errorf("message = %q, want the fallback reason", old.GetInstance().GetStatus().GetMessage())
	}

	h.now = h.now.Add(6 * time.Second)
	h.pass()
	insts := h.instances()
	if len(insts) != 1 || insts[0].GetMeta().GetName() != surge.GetMeta().GetName() {
		t.Fatalf("instances = %v, want only the surge", names(insts))
	}
	h.pass() // no churn afterwards
	if got := len(h.instances()); got != 1 {
		t.Fatalf("instances = %d, want 1", got)
	}
}

// A DRAINING node makes its current-hash instances retiring, so the same
// surge-first dance moves them elsewhere.
func TestNodeDrainRetiresInstances(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v1", 5)
	hash := hashOf(t, wl)
	h.put(wl)
	h.put(&klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
		Meta:   &klitev1.Meta{Name: "node-2"},
		Spec:   &klitev1.NodeSpec{MaxInstances: 8},
		Status: &klitev1.NodeStatus{Phase: klitev1.NodePhase_NODE_PHASE_DRAINING, Unschedulable: true},
	}}})
	h.seedInstance("b", "b-old", hash, "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	h.pass()
	if got := len(h.instances()); got != 2 {
		t.Fatalf("instances = %d, want surge alongside the old one", got)
	}
	surge := h.firstWhere(func(i *klitev1.Instance) bool { return i.GetMeta().GetName() != "b-old" })
	// The scheduler binds the surge elsewhere and the agent reports it READY.
	obj, rev, err := h.st.Get(h.ctx, object.KindInstance, surge.GetMeta().GetName())
	if err != nil {
		t.Fatal(err)
	}
	obj.GetInstance().Spec.Node = "node-1"
	obj.GetInstance().Status.Phase = klitev1.InstancePhase_INSTANCE_PHASE_READY
	if _, err := h.st.Put(h.ctx, obj, rev); err != nil {
		t.Fatal(err)
	}

	h.pass()
	old, _, err := h.st.Get(h.ctx, object.KindInstance, "b-old")
	if err != nil {
		t.Fatal(err)
	}
	if old.GetInstance().GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_DRAINING {
		t.Fatalf("old phase = %v, want DRAINING", old.GetInstance().GetStatus().GetPhase())
	}
	h.now = h.now.Add(6 * time.Second)
	h.pass()
	insts := h.instances()
	if len(insts) != 1 || insts[0].GetSpec().GetNode() != "node-1" {
		t.Fatalf("instances = %v, want only the replacement on node-1", names(insts))
	}
}

// TestLeaderFailoverMidRolloutRestartsClock hands a half-finished rollout to
// a successor controller. The successor must restart the victim's drain clock
// rather than resume the old deadline, must not delete it early, and must not
// surge a second time for the same step.
func TestLeaderFailoverMidRolloutRestartsClock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v2", 5)
	newHash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-old", "h-old", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	h.pass() // surge created
	surge := h.firstWhere(func(i *klitev1.Instance) bool { return i.GetSpec().GetTemplateHash() == newHash })
	h.setPhase(surge.GetMeta().GetName(), klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.pass() // old marked DRAINING, deadline now+5s in the first leader's memory

	// 4s later the leader dies. The successor starts with empty deadlines.
	h.now = h.now.Add(4 * time.Second)
	h.c = newWorkloadController(h.st)
	h.c.now = func() time.Time { return h.now }
	h.pass() // successor sees the drain and restarts its clock

	// 4s into the restarted clock the original deadline is long past, but
	// the instance must survive: restart means restart, not resume.
	h.now = h.now.Add(4 * time.Second)
	h.pass()
	if got := len(h.instances()); got != 2 {
		t.Fatalf("instances = %d, drain expired on the dead leader's clock", got)
	}
	if got := h.count(newHash, klitev1.InstancePhase_INSTANCE_PHASE_READY); got != 1 {
		t.Fatalf("fresh ready = %d, failover must not spawn a second surge", got)
	}

	// The restarted clock expires 5s after the successor first saw the drain.
	h.now = h.now.Add(2 * time.Second)
	h.pass()
	insts := h.instances()
	if len(insts) != 1 || insts[0].GetMeta().GetName() != surge.GetMeta().GetName() {
		t.Fatalf("instances = %v, want only the surge after the restarted drain", names(insts))
	}
}

// TestScaleDownDuringRollout shrinks replicas and changes the template in one
// edit. Old instances must leave one at a time with no surge until the count
// fits, and the workload must never serve fewer than the new replica count.
func TestScaleDownDuringRollout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 2, "v2", 5)
	newHash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-old1", "h-old", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-old2", "h-old", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-old3", "h-old", "node-3", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	serving := func() int {
		return h.count("", klitev1.InstancePhase_INSTANCE_PHASE_READY) +
			h.count("", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING)
	}
	h.pass()
	if got := h.count("", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING); got != 1 {
		t.Fatalf("draining = %d, want the surplus retired one at a time", got)
	}
	if got := h.count(newHash, klitev1.InstancePhase_INSTANCE_PHASE_PENDING); got != 0 {
		t.Fatalf("fresh created = %d, want no surge while over the new count", got)
	}
	if got := serving(); got < 2 {
		t.Fatalf("serving = %d, dipped below the new replica count", got)
	}

	h.now = h.now.Add(6 * time.Second)
	h.pass() // the drain expires and the victim is deleted
	h.pass() // at the target count the surge-first dance resumes
	if got := h.count(newHash, klitev1.InstancePhase_INSTANCE_PHASE_PENDING); got != 1 {
		t.Fatalf("surge count = %d, want exactly one once the count fits", got)
	}
	if got := serving(); got < 2 {
		t.Fatalf("serving = %d, dipped below replicas during the surge", got)
	}
}

// TestScaleUpDuringRolloutRefillsInBulk raises replicas alongside a template
// change. Restoring base capacity is not a rolling update, so the shortfall
// arrives as one batch of fresh instances.
func TestScaleUpDuringRolloutRefillsInBulk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 3, "v2", 5)
	newHash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-old", "h-old", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	h.pass()
	if got := h.count(newHash, klitev1.InstancePhase_INSTANCE_PHASE_PENDING); got != 2 {
		t.Fatalf("fresh created = %d, want the shortfall filled in one pass", got)
	}
	old, _, err := h.st.Get(h.ctx, object.KindInstance, "b-old")
	if err != nil {
		t.Fatal(err)
	}
	if old.GetInstance().GetStatus().GetPhase() != klitev1.InstancePhase_INSTANCE_PHASE_READY {
		t.Fatalf("old phase = %v, must keep serving through the refill", old.GetInstance().GetStatus().GetPhase())
	}
}

// The drain bookkeeping must not outlive the instances it tracks, or a long
// leadership accumulates dead entries.
func TestDeadlinesPrunedWithInstances(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v1", 5)
	hash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-aa", hash, "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-bb", hash, "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY)

	h.pass() // surplus b-bb goes DRAINING, deadline recorded
	if len(h.c.deadlines) != 1 {
		t.Fatalf("deadlines = %d, want the one draining instance", len(h.c.deadlines))
	}
	h.now = h.now.Add(6 * time.Second)
	h.pass() // drain expires, instance deleted
	h.pass() // the next pass prunes the dead entry
	if len(h.c.deadlines) != 0 {
		t.Fatalf("deadlines = %d, want the map empty after deletion", len(h.c.deadlines))
	}
}

// A fresh leader that finds an already-DRAINING instance restarts its clock
// instead of deleting it immediately.
func TestUnknownDrainDeadlineRestartsClock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wl := workloadObj("b", 1, "v1", 5)
	hash := hashOf(t, wl)
	h.put(wl)
	h.seedInstance("b", "b-aa", hash, "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY)
	h.seedInstance("b", "b-bb", hash, "node-2", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING)

	h.pass()
	if got := len(h.instances()); got != 2 {
		t.Fatalf("instances = %d, the unknown drain must survive the first pass", got)
	}
	h.now = h.now.Add(6 * time.Second)
	h.pass()
	if got := len(h.instances()); got != 1 {
		t.Fatalf("instances = %d, want the drain expired on the restarted clock", got)
	}
}
