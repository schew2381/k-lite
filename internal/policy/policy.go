// Package policy evaluates NetworkPolicy objects with istio-lite semantics
// (ADR 0009): DENY rules always win, and a Service flips to allowlist mode
// once any ALLOW rule targets it. All of it is pure functions, shared by the
// xDS RBAC compiler, the PolicyCheck RPC, and the UI simulator.
package policy

import (
	"cmp"
	"fmt"
	"slices"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Decision is the outcome of evaluating one from->to connection.
type Decision struct {
	Allowed       bool
	MatchedPolicy string
	Reason        string
}

// Evaluate decides whether traffic from service `from` to service `to` is
// admitted under the given policies. Service names are opaque strings.
func Evaluate(policies []*klitev1.NetworkPolicy, from, to string) Decision {
	sorted := sortedByName(policies)

	// DENY rules always win.
	for _, p := range sorted {
		if p.GetSpec().GetAction() != klitev1.PolicyAction_POLICY_ACTION_DENY {
			continue
		}
		for i, r := range p.GetSpec().GetRules() {
			if ruleMatches(r, from, to) {
				name := p.GetMeta().GetName()
				return Decision{
					Allowed:       false,
					MatchedPolicy: name,
					Reason:        fmt.Sprintf("denied by %s rule %d", name, i+1),
				}
			}
		}
	}

	// Any ALLOW rule targeting `to` flips it from default-open to
	// allowlist mode.
	allowlisted := false
	for _, p := range sorted {
		if p.GetSpec().GetAction() != klitev1.PolicyAction_POLICY_ACTION_ALLOW {
			continue
		}
		for i, r := range p.GetSpec().GetRules() {
			if !ruleTargets(r, to) {
				continue
			}
			allowlisted = true
			if ruleFromMatches(r, from) {
				name := p.GetMeta().GetName()
				return Decision{
					Allowed:       true,
					MatchedPolicy: name,
					Reason:        fmt.Sprintf("allowed by %s rule %d", name, i+1),
				}
			}
		}
	}
	if !allowlisted {
		return Decision{Allowed: true, Reason: fmt.Sprintf("no ALLOW targets %s, default allow", to)}
	}
	return Decision{
		Allowed: false,
		Reason:  fmt.Sprintf("%s is allowlist-mode and no ALLOW admits %s", to, from),
	}
}

// Compile flattens policies into the wire form consumed by klite-net and
// Envoy RBAC: one CompiledPolicy per rule, ordered by policy name then rule
// index.
func Compile(policies []*klitev1.NetworkPolicy) []*klitev1.CompiledPolicy {
	var out []*klitev1.CompiledPolicy
	for _, p := range sortedByName(policies) {
		for _, r := range p.GetSpec().GetRules() {
			out = append(out, &klitev1.CompiledPolicy{
				Action:     p.GetSpec().GetAction(),
				From:       r.GetFrom(),
				To:         r.GetTo(),
				Except:     slices.Clone(r.GetExcept()),
				PolicyName: p.GetMeta().GetName(),
			})
		}
	}
	return out
}

func sortedByName(policies []*klitev1.NetworkPolicy) []*klitev1.NetworkPolicy {
	s := slices.Clone(policies)
	slices.SortStableFunc(s, func(a, b *klitev1.NetworkPolicy) int {
		return cmp.Compare(a.GetMeta().GetName(), b.GetMeta().GetName())
	})
	return s
}

func ruleMatches(r *klitev1.PolicyRule, from, to string) bool {
	return ruleFromMatches(r, from) && ruleTargets(r, to)
}

func ruleFromMatches(r *klitev1.PolicyRule, from string) bool {
	return r.GetFrom() == "*" || r.GetFrom() == from
}

// ruleTargets reports whether the rule's destination covers `to`, consulting
// the except list only on the wildcard.
func ruleTargets(r *klitev1.PolicyRule, to string) bool {
	if r.GetTo() == to {
		return true
	}
	return r.GetTo() == "*" && !slices.Contains(r.GetExcept(), to)
}
