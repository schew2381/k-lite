package controller

import (
	"slices"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func instance(name, hash string, createdUnix, rev int64) *klitev1.Instance {
	return &klitev1.Instance{
		Meta: &klitev1.Meta{Name: name, CreatedUnix: createdUnix, ResourceVersion: rev},
		Spec: &klitev1.InstanceSpec{Workload: "b", TemplateHash: hash},
	}
}

func names(instances []*klitev1.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, in := range instances {
		out = append(out, in.GetMeta().GetName())
	}
	slices.Sort(out)
	return out
}

func TestDiffInstances(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		replicas   int
		hash       string
		instances  []*klitev1.Instance
		wantCreate int
		wantDelete []string
	}{
		{
			name:       "steady state",
			replicas:   2,
			hash:       "h1",
			instances:  []*klitev1.Instance{instance("b-aa", "h1", 10, 1), instance("b-bb", "h1", 11, 2)},
			wantCreate: 0,
		},
		{
			name:       "scale up from zero",
			replicas:   3,
			hash:       "h1",
			wantCreate: 3,
		},
		{
			name:     "scale down deletes newest first",
			replicas: 1,
			hash:     "h1",
			instances: []*klitev1.Instance{
				instance("b-old", "h1", 10, 1),
				instance("b-mid", "h1", 20, 2),
				instance("b-new", "h1", 30, 3),
			},
			wantDelete: []string{"b-mid", "b-new"},
		},
		{
			name:     "same-second creations fall back to revision",
			replicas: 1,
			hash:     "h1",
			instances: []*klitev1.Instance{
				instance("b-r5", "h1", 10, 5),
				instance("b-r9", "h1", 10, 9),
			},
			wantDelete: []string{"b-r9"},
		},
		{
			name:     "hash change replaces everything",
			replicas: 2,
			hash:     "h2",
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", 10, 1),
				instance("b-bb", "h1", 11, 2),
			},
			wantCreate: 2,
			wantDelete: []string{"b-aa", "b-bb"},
		},
		{
			name:     "hash change mid-flight keeps current-hash instances",
			replicas: 2,
			hash:     "h2",
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", 10, 1),
				instance("b-cc", "h2", 12, 3),
			},
			wantCreate: 1,
			wantDelete: []string{"b-aa"},
		},
		{
			name:     "scale to zero",
			replicas: 0,
			hash:     "h1",
			instances: []*klitev1.Instance{
				instance("b-aa", "h1", 10, 1),
			},
			wantDelete: []string{"b-aa"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			create, del := diffInstances(tt.replicas, tt.hash, tt.instances)
			if create != tt.wantCreate {
				t.Errorf("create = %d, want %d", create, tt.wantCreate)
			}
			got := names(del)
			want := slices.Clone(tt.wantDelete)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("delete = %v, want %v", got, want)
			}
		})
	}
}
