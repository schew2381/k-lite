package cli

import (
	"strings"
	"testing"
	"time"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

func TestDescribeWorkloadGolden(t *testing.T) {
	t.Parallel()
	now := time.Unix(1120, 0) // 2m after creation
	obj := &klitev1.Object{Kind: &klitev1.Object_Workload{Workload: &klitev1.Workload{
		Meta: &klitev1.Meta{Name: "a", Labels: map[string]string{"app": "a"}, CreatedUnix: 1000},
		Spec: &klitev1.WorkloadSpec{
			Replicas: 2,
			Template: &klitev1.Template{Containers: []*klitev1.Container{{Name: "client", Image: "alpine:3.20"}}},
			Drain:    &klitev1.DrainSpec{DrainTimeoutSeconds: 30, TerminationGraceSeconds: 15},
		},
		Status: &klitev1.WorkloadStatus{ReadyInstances: 1, TemplateHash: "h-abc123"},
	}}}
	related := []*klitev1.Instance{
		{
			Meta:   &klitev1.Meta{Name: "a-aaaaa"},
			Spec:   &klitev1.InstanceSpec{Workload: "a", Node: "node-1"},
			Status: &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_RUNNING, Restarts: 1},
		},
		{
			Meta:   &klitev1.Meta{Name: "a-bbbbb"},
			Spec:   &klitev1.InstanceSpec{Workload: "a"},
			Status: &klitev1.InstanceStatus{Phase: klitev1.InstancePhase_INSTANCE_PHASE_PENDING},
		},
	}

	var out strings.Builder
	if err := describe(&out, obj, related, now); err != nil {
		t.Fatal(err)
	}
	want := `Name:           a
Labels:         app=a
Age:            2m
Replicas:       2 desired, 1 ready
Node pin:       -
Container:      client (alpine:3.20)
Template hash:  h-abc123
Drain:          timeout 30s, grace 15s
Instances:
  NAME      NODE     PHASE     RESTARTS
  a-aaaaa   node-1   Running   1
  a-bbbbb   -        Pending   0
`
	if out.String() != want {
		t.Errorf("describe workload output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestDescribeInstanceShowsPlacement(t *testing.T) {
	t.Parallel()
	obj := &klitev1.Object{Kind: &klitev1.Object_Instance{Instance: &klitev1.Instance{
		Meta: &klitev1.Meta{Name: "a-aaaaa", CreatedUnix: 1000},
		Spec: &klitev1.InstanceSpec{Workload: "a", Node: "node-2"},
		Status: &klitev1.InstanceStatus{
			Phase:       klitev1.InstancePhase_INSTANCE_PHASE_PENDING,
			InstanceIp:  "10.44.128.7",
			ContainerId: "0123456789abcdef",
			Message:     "no ready schedulable node with free capacity",
		},
	}}}

	var out strings.Builder
	if err := describe(&out, obj, nil, time.Unix(1060, 0)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Node:       node-2",
		"Phase:      Pending",
		"IP:         10.44.128.7",
		"Container:  0123456789ab",
		"Message:    no ready schedulable node with free capacity",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("describe instance output missing %q:\n%s", want, out.String())
		}
	}
}
