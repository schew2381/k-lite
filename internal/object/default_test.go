package object_test

import (
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

func TestDefaultWorkloadDrain(t *testing.T) {
	tests := []struct {
		name      string
		drain     *klitev1.DrainSpec
		wantDrain int32
		wantGrace int32
	}{
		{"nil drain", nil, 30, 15},
		{"zero fields", &klitev1.DrainSpec{}, 30, 15},
		{"timeout set", &klitev1.DrainSpec{DrainTimeoutSeconds: 60}, 60, 15},
		{"grace set", &klitev1.DrainSpec{TerminationGraceSeconds: 5}, 30, 5},
		{"both set", &klitev1.DrainSpec{DrainTimeoutSeconds: 60, TerminationGraceSeconds: 5}, 60, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := workload("b", 1, &klitev1.Container{Name: "web"})
			o.GetWorkload().Spec.Drain = tt.drain
			object.Default(o)
			d := o.GetWorkload().GetSpec().GetDrain()
			if d.GetDrainTimeoutSeconds() != tt.wantDrain || d.GetTerminationGraceSeconds() != tt.wantGrace {
				t.Errorf("drain = %d/%d, want %d/%d",
					d.GetDrainTimeoutSeconds(), d.GetTerminationGraceSeconds(), tt.wantDrain, tt.wantGrace)
			}
		})
	}
}

func TestDefaultServiceTargetPort(t *testing.T) {
	tests := []struct {
		name       string
		port       int32
		targetPort int32
		want       int32
	}{
		{"unset falls back to port", 8080, 0, 8080},
		{"explicit value stays", 8080, 80, 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := service("b", tt.port, tt.targetPort)
			object.Default(o)
			if got := o.GetService().GetSpec().GetTargetPort(); got != tt.want {
				t.Errorf("targetPort = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDefaultNodeMaxInstances(t *testing.T) {
	tests := []struct {
		name string
		in   int32
		want int32
	}{
		{"unset becomes 32", 0, 32},
		{"explicit value stays", 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &klitev1.Object{Kind: &klitev1.Object_Node{Node: &klitev1.Node{
				Meta: &klitev1.Meta{Name: "node-1"},
				Spec: &klitev1.NodeSpec{MaxInstances: tt.in},
			}}}
			object.Default(o)
			if got := o.GetNode().GetSpec().GetMaxInstances(); got != tt.want {
				t.Errorf("maxInstances = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDefaultIsIdempotent(t *testing.T) {
	o := workload("b", 1, &klitev1.Container{Name: "web"})
	object.Default(o)
	first := o.GetWorkload().GetSpec().GetDrain()
	object.Default(o)
	second := o.GetWorkload().GetSpec().GetDrain()
	if first.GetDrainTimeoutSeconds() != second.GetDrainTimeoutSeconds() ||
		first.GetTerminationGraceSeconds() != second.GetTerminationGraceSeconds() {
		t.Errorf("second Default changed drain: %v then %v", first, second)
	}
}
