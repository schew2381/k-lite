package object

import (
	"fmt"
	"regexp"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// dnsLabel matches an RFC 1123 label: lowercase alphanumerics and dashes, starting and ending alphanumeric.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxNameLen = 63

// Validate rejects objects the store must never hold. Run it after Default so range checks see filled-in values.
func Validate(o *klitev1.Object) error {
	kind := KindOf(o)
	if kind == "" {
		return fmt.Errorf("empty object envelope")
	}
	name := MetaOf(o).GetName()
	if err := validateName(name); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	var err error
	switch k := o.GetKind().(type) {
	case *klitev1.Object_Workload:
		err = validateWorkload(k.Workload)
	case *klitev1.Object_Service:
		err = validateService(k.Service)
	case *klitev1.Object_NetworkPolicy:
		err = validatePolicy(k.NetworkPolicy)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", kind, name, err)
	}
	return nil
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("name %q exceeds %d characters", name, maxNameLen)
	}
	if !dnsLabel.MatchString(name) {
		return fmt.Errorf("name %q must be a DNS label (lowercase alphanumerics and dashes)", name)
	}
	return nil
}

func validateWorkload(w *klitev1.Workload) error {
	spec := w.GetSpec()
	if spec.GetReplicas() < 0 {
		return fmt.Errorf("replicas must be >= 0, got %d", spec.GetReplicas())
	}
	// A template holds exactly one container (ADR 0014). The list shape survives but a second entry doesn't.
	if n := len(spec.GetTemplate().GetContainers()); n != 1 {
		return fmt.Errorf("template must hold exactly one container, got %d", n)
	}
	return nil
}

func validateService(s *klitev1.Service) error {
	spec := s.GetSpec()
	if p := spec.GetPort(); p < 1 || p > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", p)
	}
	if tp := spec.GetTargetPort(); tp < 1 || tp > 65535 {
		return fmt.Errorf("targetPort %d is outside 1-65535", tp)
	}
	return nil
}

func validatePolicy(p *klitev1.NetworkPolicy) error {
	spec := p.GetSpec()
	switch spec.GetAction() {
	case klitev1.PolicyAction_POLICY_ACTION_ALLOW, klitev1.PolicyAction_POLICY_ACTION_DENY:
	default:
		return fmt.Errorf("action must be ALLOW or DENY")
	}
	for i, r := range spec.GetRules() {
		if r.GetFrom() == "" {
			return fmt.Errorf("rule %d: from is required (\"*\" matches everything)", i+1)
		}
		if r.GetTo() == "" {
			return fmt.Errorf("rule %d: to is required (\"*\" matches everything)", i+1)
		}
		if len(r.GetExcept()) > 0 && r.GetTo() != "*" {
			return fmt.Errorf("rule %d: except only applies when to is \"*\"", i+1)
		}
	}
	return nil
}
