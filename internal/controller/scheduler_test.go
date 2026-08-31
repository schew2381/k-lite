package controller

import (
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func node(name string, phase klitev1.NodePhase, unschedulable bool, maxInstances int32) *klitev1.Node {
	return &klitev1.Node{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.NodeSpec{MaxInstances: maxInstances},
		Status: &klitev1.NodeStatus{
			Phase:         phase,
			Unschedulable: unschedulable,
		},
	}
}

func TestPickNode(t *testing.T) {
	t.Parallel()
	ready := klitev1.NodePhase_NODE_PHASE_READY
	notReady := klitev1.NodePhase_NODE_PHASE_NOT_READY

	tests := []struct {
		name       string
		pin        string
		nodes      []*klitev1.Node
		counts     map[string]int
		want       string
		wantReason bool
	}{
		{
			name: "spread picks fewest instances",
			nodes: []*klitev1.Node{
				node("node-1", ready, false, 32),
				node("node-2", ready, false, 32),
				node("node-3", ready, false, 32),
			},
			counts: map[string]int{"node-1": 2, "node-2": 2, "node-3": 1},
			want:   "node-3",
		},
		{
			name: "tie breaks by node name",
			nodes: []*klitev1.Node{
				node("node-2", ready, false, 32),
				node("node-1", ready, false, 32),
			},
			counts: map[string]int{"node-1": 1, "node-2": 1},
			want:   "node-1",
		},
		{
			name: "pin wins over emptier nodes",
			pin:  "node-2",
			nodes: []*klitev1.Node{
				node("node-1", ready, false, 32),
				node("node-2", ready, false, 32),
			},
			counts: map[string]int{"node-1": 0, "node-2": 9},
			want:   "node-2",
		},
		{
			name: "pin to cordoned node stays pending",
			pin:  "node-1",
			nodes: []*klitev1.Node{
				node("node-1", ready, true, 32),
				node("node-2", ready, false, 32),
			},
			counts:     map[string]int{},
			wantReason: true,
		},
		{
			name: "cordoned node is skipped",
			nodes: []*klitev1.Node{
				node("node-1", ready, true, 32),
				node("node-2", ready, false, 32),
			},
			counts: map[string]int{"node-1": 0, "node-2": 5},
			want:   "node-2",
		},
		{
			name: "not-ready node is skipped",
			nodes: []*klitev1.Node{
				node("node-1", notReady, false, 32),
				node("node-2", ready, false, 32),
			},
			counts: map[string]int{"node-1": 0, "node-2": 5},
			want:   "node-2",
		},
		{
			name: "node at capacity is skipped",
			nodes: []*klitev1.Node{
				node("node-1", ready, false, 2),
				node("node-2", ready, false, 32),
			},
			counts: map[string]int{"node-1": 2, "node-2": 5},
			want:   "node-2",
		},
		{
			name: "nothing schedulable",
			nodes: []*klitev1.Node{
				node("node-1", notReady, false, 32),
				node("node-2", ready, true, 32),
				node("node-3", ready, false, 1),
			},
			counts:     map[string]int{"node-3": 1},
			wantReason: true,
		},
		{
			name:       "no nodes at all",
			nodes:      nil,
			counts:     map[string]int{},
			wantReason: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason := pickNode(tt.pin, tt.nodes, tt.counts)
			if got != tt.want {
				t.Errorf("pickNode() = %q, want %q (reason %q)", got, tt.want, reason)
			}
			if tt.wantReason && reason == "" {
				t.Error("pickNode() returned no reason for an unschedulable instance")
			}
			if !tt.wantReason && reason != "" {
				t.Errorf("pickNode() returned unexpected reason %q", reason)
			}
		})
	}
}

func TestPickNodeSpreadsFive(t *testing.T) {
	t.Parallel()
	nodes := []*klitev1.Node{
		node("node-1", klitev1.NodePhase_NODE_PHASE_READY, false, 32),
		node("node-2", klitev1.NodePhase_NODE_PHASE_READY, false, 32),
		node("node-3", klitev1.NodePhase_NODE_PHASE_READY, false, 32),
	}
	counts := map[string]int{}
	for range 5 {
		got, reason := pickNode("", nodes, counts)
		if got == "" {
			t.Fatalf("pickNode() found nothing: %s", reason)
		}
		counts[got]++
	}
	want := map[string]int{"node-1": 2, "node-2": 2, "node-3": 1}
	for n, c := range want {
		if counts[n] != c {
			t.Errorf("counts[%s] = %d, want %d (all: %v)", n, counts[n], c, counts)
		}
	}
}
