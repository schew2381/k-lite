package object

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// dnsLabel matches an RFC 1123 label: lowercase alphanumerics and dashes, starting and ending alphanumeric.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	maxNameLen  = 63
	maxLabelLen = 63
	maxPort     = 65535
	// maxReplicas keeps a typo'd replica count from flooding etcd with
	// Instance objects. k-lite is a ~100-instance system (ADR 0005).
	maxReplicas = 1000
)

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
	err := validateLabels("metadata.labels", MetaOf(o).GetLabels())
	if err == nil {
		switch k := o.GetKind().(type) {
		case *klitev1.Object_Workload:
			err = validateWorkload(k.Workload)
		case *klitev1.Object_Service:
			err = validateService(k.Service)
		case *klitev1.Object_Node:
			err = validateNode(k.Node)
		case *klitev1.Object_NetworkPolicy:
			err = validatePolicy(k.NetworkPolicy)
		}
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

// validateLabels enforces sane bounds on label maps: no empty keys, nothing
// over 63 characters. Selectors match by string equality, so anything within
// those bounds works. Keys are checked in sorted order for stable errors.
func validateLabels(field string, labels map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if k == "" {
			return fmt.Errorf("%s: label key must not be empty", field)
		}
		if len(k) > maxLabelLen {
			return fmt.Errorf("%s: label key %q exceeds %d characters", field, k, maxLabelLen)
		}
		if v := labels[k]; len(v) > maxLabelLen {
			return fmt.Errorf("%s: value of label %q exceeds %d characters", field, k, maxLabelLen)
		}
	}
	return nil
}

func validatePort(field string, p int32) error {
	if p < 1 || p > maxPort {
		return fmt.Errorf("%s %d is outside 1-%d", field, p, maxPort)
	}
	return nil
}

func validateWorkload(w *klitev1.Workload) error {
	spec := w.GetSpec()
	if r := spec.GetReplicas(); r < 0 || r > maxReplicas {
		return fmt.Errorf("replicas must be 0-%d, got %d", maxReplicas, r)
	}
	if n := spec.GetNodeName(); n != "" {
		if err := validateName(n); err != nil {
			return fmt.Errorf("nodeName: %w", err)
		}
	}
	tpl := spec.GetTemplate()
	if err := validateLabels("template.labels", tpl.GetLabels()); err != nil {
		return err
	}
	// A template holds exactly one container (ADR 0014). The list shape survives but a second entry doesn't.
	if n := len(tpl.GetContainers()); n != 1 {
		return fmt.Errorf("template must hold exactly one container, got %d", n)
	}
	return validateContainer(tpl.GetContainers()[0])
}

func validateContainer(c *klitev1.Container) error {
	if c.GetImage() == "" {
		return fmt.Errorf("container image is required")
	}
	for i, e := range c.GetEnv() {
		if e.GetName() == "" {
			return fmt.Errorf("env %d: name is required", i+1)
		}
	}
	for _, p := range c.GetPorts() {
		if err := validatePort("containerPort", p.GetContainerPort()); err != nil {
			return err
		}
	}
	if probe := c.GetReadinessProbe(); probe != nil {
		return validatePort("readinessProbe.tcpPort", probe.GetTcpPort())
	}
	return nil
}

func validateService(s *klitev1.Service) error {
	spec := s.GetSpec()
	if err := validateLabels("spec.selector", spec.GetSelector()); err != nil {
		return err
	}
	if err := validatePort("port", spec.GetPort()); err != nil {
		return err
	}
	return validatePort("targetPort", spec.GetTargetPort())
}

func validateNode(n *klitev1.Node) error {
	// Zero is fine here: Default turns it into 32 before this runs.
	if m := n.GetSpec().GetMaxInstances(); m < 0 {
		return fmt.Errorf("maxInstances must be >= 0, got %d", m)
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
