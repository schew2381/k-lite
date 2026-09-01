package controller

import (
	"context"
	"slices"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/store/storetest"
)

// instance builds a bare instance for classification tests. rev doubles as
// the age tiebreak: higher rev sorts newer.
func instance(name, hash, node string, phase klitev1.InstancePhase, rev int64) *klitev1.Instance {
	return &klitev1.Instance{
		Meta:   &klitev1.Meta{Name: name, Uid: "uid-" + name, ResourceVersion: rev},
		Spec:   &klitev1.InstanceSpec{Workload: "b", TemplateHash: hash, Node: node},
		Status: &klitev1.InstanceStatus{Phase: phase},
	}
}

func names(instances []*klitev1.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, in := range instances {
		out = append(out, in.GetMeta().GetName())
	}
	return out
}

// M5 note: these cases replace the old diffInstances tests. Deletions of
// serving instances became retirements (drain first), so the buckets carry
// what diffInstances' create/delete counts used to.
func TestClassify(t *testing.T) {
	t.Parallel()
	ready := klitev1.InstancePhase_INSTANCE_PHASE_READY
	pending := klitev1.InstancePhase_INSTANCE_PHASE_PENDING
	draining := klitev1.InstancePhase_INSTANCE_PHASE_DRAINING
	tests := []struct {
		name          string
		hash          string
		drainingNodes map[string]bool
		instances     []*klitev1.Instance
		fresh         []string
		retiring      []string
		doomed        []string
		drainingSet   []string
	}{
		{
			name: "steady state",
			hash: "h1",
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", "node-1", ready, 1),
				instance("b-bb", "h1", "node-2", ready, 2),
			},
			fresh: []string{"b-bb", "b-aa"},
		},
		{
			name: "hash change: serving instances retire newest-first, pending ones are doomed",
			hash: "h2",
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", "node-1", ready, 1),
				instance("b-bb", "h1", "node-2", ready, 2),
				instance("b-cc", "h1", "", pending, 3),
			},
			retiring: []string{"b-bb", "b-aa"},
			doomed:   []string{"b-cc"},
		},
		{
			name:          "draining node retires current-hash instances",
			hash:          "h1",
			drainingNodes: map[string]bool{"node-2": true},
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", "node-1", ready, 1),
				instance("b-bb", "h1", "node-2", ready, 2),
			},
			fresh:    []string{"b-aa"},
			retiring: []string{"b-bb"},
		},
		{
			name: "draining phase wins over everything",
			hash: "h1",
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", "node-1", draining, 1),
				instance("b-bb", "h9", "node-2", draining, 2),
			},
			drainingSet: []string{"b-aa", "b-bb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := classify(tt.hash, tt.instances, tt.drainingNodes)
			for _, cmp := range []struct {
				field string
				got   []*klitev1.Instance
				want  []string
			}{
				{"fresh", g.fresh, tt.fresh},
				{"retiring", g.retiring, tt.retiring},
				{"doomed", g.doomed, tt.doomed},
				{"draining", g.draining, tt.drainingSet},
			} {
				got := names(cmp.got)
				want := cmp.want
				if cmp.field == "doomed" || cmp.field == "draining" {
					slices.Sort(got)
					want = slices.Clone(want)
					slices.Sort(want)
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s = %v, want %v", cmp.field, got, want)
				}
			}
		})
	}
}

// Ready counts every READY instance regardless of hash: old-template
// instances still serve mid-rollout, and totals never clamp to replicas.
func TestUpdateStatusCountsAllServing(t *testing.T) {
	t.Parallel()
	st := storetest.New()
	ctx := context.Background()
	obj := workloadObj("b", 2, "v2", 5)
	if _, err := st.Put(ctx, obj, -1); err != nil {
		t.Fatal(err)
	}
	obj, _, _ = st.Get(ctx, "Workload", "b")
	instances := []*klitev1.Instance{
		instance("b-old1", "h1", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY, 1),
		instance("b-old2", "h1", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_READY, 2),
		instance("b-new1", "h2", "node-1", klitev1.InstancePhase_INSTANCE_PHASE_READY, 3),
		instance("b-drain", "h1", "node-2", klitev1.InstancePhase_INSTANCE_PHASE_DRAINING, 4),
	}
	c := newWorkloadController(st)
	if err := c.updateStatus(ctx, obj, "h2", instances); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.Get(ctx, "Workload", "b")
	if err != nil {
		t.Fatal(err)
	}
	s := got.GetWorkload().GetStatus()
	if s.GetReadyInstances() != 3 || s.GetTotalInstances() != 4 || s.GetTemplateHash() != "h2" {
		t.Errorf("status = %v, want ready 3 total 4 hash h2", s)
	}
}
