package object

import (
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Fields users omit get these values. A written zero gets them too, since proto3 can't tell the two apart.
const (
	DefaultDrainTimeoutSeconds     = 30
	DefaultTerminationGraceSeconds = 15
	DefaultMaxInstances            = 32
)

// Default fills omitted spec fields in place. It's idempotent, so re-applying stored objects changes nothing.
func Default(o *klitev1.Object) {
	switch k := o.GetKind().(type) {
	case *klitev1.Object_Workload:
		defaultWorkload(k.Workload)
	case *klitev1.Object_Service:
		defaultService(k.Service)
	case *klitev1.Object_Node:
		defaultNode(k.Node)
	}
}

func defaultWorkload(w *klitev1.Workload) {
	if w.Spec == nil {
		w.Spec = &klitev1.WorkloadSpec{}
	}
	if w.Spec.Drain == nil {
		w.Spec.Drain = &klitev1.DrainSpec{}
	}
	if w.Spec.Drain.DrainTimeoutSeconds == 0 {
		w.Spec.Drain.DrainTimeoutSeconds = DefaultDrainTimeoutSeconds
	}
	if w.Spec.Drain.TerminationGraceSeconds == 0 {
		w.Spec.Drain.TerminationGraceSeconds = DefaultTerminationGraceSeconds
	}
}

func defaultService(s *klitev1.Service) {
	if s.Spec == nil {
		s.Spec = &klitev1.ServiceSpec{}
	}
	if s.Spec.TargetPort == 0 {
		s.Spec.TargetPort = s.Spec.Port
	}
}

func defaultNode(n *klitev1.Node) {
	if n.Spec == nil {
		n.Spec = &klitev1.NodeSpec{}
	}
	if n.Spec.MaxInstances == 0 {
		n.Spec.MaxInstances = DefaultMaxInstances
	}
}
