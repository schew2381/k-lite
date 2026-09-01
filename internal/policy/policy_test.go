package policy_test

import (
	"slices"
	"testing"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/policy"
	"google.golang.org/protobuf/proto"
)

const (
	allow = klitev1.PolicyAction_POLICY_ACTION_ALLOW
	deny  = klitev1.PolicyAction_POLICY_ACTION_DENY
)

func pol(name string, action klitev1.PolicyAction, rules ...*klitev1.PolicyRule) *klitev1.NetworkPolicy {
	return &klitev1.NetworkPolicy{
		Meta: &klitev1.Meta{Name: name},
		Spec: &klitev1.NetworkPolicySpec{Action: action, Rules: rules},
	}
}

func rule(from, to string, except ...string) *klitev1.PolicyRule {
	return &klitev1.PolicyRule{From: from, To: to, Except: except}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	denyAtoC := pol("deny-a-to-c", deny, rule("a", "c"))
	lockdownA := pol("lockdown-a", deny, rule("a", "*", "b"))
	allowOnlyAtoB := pol("allow-only-a-to-b", allow, rule("a", "b"))
	allowABroad := pol("allow-a-broad", allow, rule("a", "*", "b"))

	tests := []struct {
		name     string
		policies []*klitev1.NetworkPolicy
		from, to string
		want     policy.Decision
	}{
		{
			name: "empty policy set default allows",
			from: "a", to: "b",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets b, default allow"},
		},
		{
			name:     "self traffic default allows",
			policies: []*klitev1.NetworkPolicy{denyAtoC},
			from:     "a", to: "a",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets a, default allow"},
		},

		// Canonical: deny a->c while a->b flows.
		{
			name:     "deny a to c blocks a to c",
			policies: []*klitev1.NetworkPolicy{denyAtoC},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-a-to-c", Reason: "denied by deny-a-to-c rule 1"},
		},
		{
			name:     "deny a to c leaves a to b open",
			policies: []*klitev1.NetworkPolicy{denyAtoC},
			from:     "a", to: "b",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets b, default allow"},
		},
		{
			name:     "deny a to c leaves b to c open",
			policies: []*klitev1.NetworkPolicy{denyAtoC},
			from:     "b", to: "c",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets c, default allow"},
		},

		// Canonical: lockdown-a via to:"*" except [b].
		{
			name:     "lockdown-a blocks a to c",
			policies: []*klitev1.NetworkPolicy{lockdownA},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "lockdown-a", Reason: "denied by lockdown-a rule 1"},
		},
		{
			name:     "lockdown-a exempts a to b",
			policies: []*klitev1.NetworkPolicy{lockdownA},
			from:     "a", to: "b",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets b, default allow"},
		},
		{
			name:     "lockdown-a leaves c to d alone",
			policies: []*klitev1.NetworkPolicy{lockdownA},
			from:     "c", to: "d",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets d, default allow"},
		},
		{
			name:     "lockdown-a wildcard catches self traffic",
			policies: []*klitev1.NetworkPolicy{lockdownA},
			from:     "a", to: "a",
			want: policy.Decision{Allowed: false, MatchedPolicy: "lockdown-a", Reason: "denied by lockdown-a rule 1"},
		},

		// Canonical: allow-only-a-to-b flips b to allowlist mode.
		{
			name:     "allow flip admits a to b",
			policies: []*klitev1.NetworkPolicy{allowOnlyAtoB},
			from:     "a", to: "b",
			want: policy.Decision{Allowed: true, MatchedPolicy: "allow-only-a-to-b", Reason: "allowed by allow-only-a-to-b rule 1"},
		},
		{
			name:     "allow flip denies c to b",
			policies: []*klitev1.NetworkPolicy{allowOnlyAtoB},
			from:     "c", to: "b",
			want: policy.Decision{Allowed: false, Reason: "b is allowlist-mode and no ALLOW admits c"},
		},
		{
			name:     "allow flip leaves c to a alone",
			policies: []*klitev1.NetworkPolicy{allowOnlyAtoB},
			from:     "c", to: "a",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets a, default allow"},
		},
		{
			name:     "allow flip catches self traffic to b",
			policies: []*klitev1.NetworkPolicy{allowOnlyAtoB},
			from:     "b", to: "b",
			want: policy.Decision{Allowed: false, Reason: "b is allowlist-mode and no ALLOW admits b"},
		},

		// Wildcard from.
		{
			name:     "wildcard from deny blocks anyone",
			policies: []*klitev1.NetworkPolicy{pol("deny-all-to-c", deny, rule("*", "c"))},
			from:     "b", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-all-to-c", Reason: "denied by deny-all-to-c rule 1"},
		},
		{
			name:     "wildcard from allow admits anyone",
			policies: []*klitev1.NetworkPolicy{pol("allow-all-to-b", allow, rule("*", "b"))},
			from:     "z", to: "b",
			want: policy.Decision{Allowed: true, MatchedPolicy: "allow-all-to-b", Reason: "allowed by allow-all-to-b rule 1"},
		},

		// DENY beats ALLOW on the same pair.
		{
			name: "deny beats allow on same pair",
			policies: []*klitev1.NetworkPolicy{
				pol("allow-a-to-b", allow, rule("a", "b")),
				pol("deny-a-to-b", deny, rule("a", "b")),
			},
			from: "a", to: "b",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-a-to-b", Reason: "denied by deny-a-to-b rule 1"},
		},

		// Wildcard ALLOW with except: b is not targeted, c is.
		{
			name:     "wildcard allow with except does not flip excepted service",
			policies: []*klitev1.NetworkPolicy{allowABroad},
			from:     "c", to: "b",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets b, default allow"},
		},
		{
			name:     "wildcard allow flips non-excepted service",
			policies: []*klitev1.NetworkPolicy{allowABroad},
			from:     "c", to: "c",
			want: policy.Decision{Allowed: false, Reason: "c is allowlist-mode and no ALLOW admits c"},
		},
		{
			name:     "wildcard allow admits a to non-excepted service",
			policies: []*klitev1.NetworkPolicy{allowABroad},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: true, MatchedPolicy: "allow-a-broad", Reason: "allowed by allow-a-broad rule 1"},
		},

		// Except is ignored on a concrete to.
		{
			name:     "except ignored when to is concrete",
			policies: []*klitev1.NetworkPolicy{pol("deny-weird", deny, rule("a", "b", "b"))},
			from:     "a", to: "b",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-weird", Reason: "denied by deny-weird rule 1"},
		},

		// Rule index in the reason is 1-based and per-policy.
		{
			name:     "second rule reports rule 2",
			policies: []*klitev1.NetworkPolicy{pol("deny-multi", deny, rule("x", "y"), rule("a", "c"))},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-multi", Reason: "denied by deny-multi rule 2"},
		},

		// Matched policy is deterministic regardless of input slice order.
		{
			name: "matched policy sorted by name",
			policies: []*klitev1.NetworkPolicy{
				pol("z-allow", allow, rule("a", "b")),
				pol("a-allow", allow, rule("a", "b")),
			},
			from: "a", to: "b",
			want: policy.Decision{Allowed: true, MatchedPolicy: "a-allow", Reason: "allowed by a-allow rule 1"},
		},

		// Unknown service names are plain strings.
		{
			name:     "unknown services default allow",
			policies: []*klitev1.NetworkPolicy{denyAtoC, allowOnlyAtoB},
			from:     "ghost", to: "phantom",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets phantom, default allow"},
		},
		{
			name:     "wildcard deny hits unknown from",
			policies: []*klitev1.NetworkPolicy{pol("deny-all-to-c", deny, rule("*", "c"))},
			from:     "ghost", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-all-to-c", Reason: "denied by deny-all-to-c rule 1"},
		},

		// With from and to both wildcarded, the only opening is the excepted destination.
		{
			name:     "deny star to star except b blocks a to c",
			policies: []*klitev1.NetworkPolicy{pol("deny-all", deny, rule("*", "*", "b"))},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-all", Reason: "denied by deny-all rule 1"},
		},
		{
			name:     "deny star to star except b lets a to b through",
			policies: []*klitev1.NetworkPolicy{pol("deny-all", deny, rule("*", "*", "b"))},
			from:     "a", to: "b",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets b, default allow"},
		},

		// Names are opaque bytes, so Unicode compares exactly.
		{
			name:     "unicode names match exactly",
			policies: []*klitev1.NetworkPolicy{pol("deny-emoji", deny, rule("café", "日本"))},
			from:     "café", to: "日本",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-emoji", Reason: "denied by deny-emoji rule 1"},
		},
		{
			name:     "unicode near-miss stays open",
			policies: []*klitev1.NetworkPolicy{pol("deny-emoji", deny, rule("café", "日本"))},
			from:     "cafe", to: "日本",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets 日本, default allow"},
		},

		// Empty query strings only ever match wildcards.
		{
			name:     "empty from matches only wildcard",
			policies: []*klitev1.NetworkPolicy{pol("deny-a-to-b", deny, rule("a", "b"))},
			from:     "", to: "b",
			want: policy.Decision{Allowed: true, Reason: "no ALLOW targets b, default allow"},
		},
		{
			name:     "wildcard deny catches empty strings",
			policies: []*klitev1.NetworkPolicy{pol("deny-everything", deny, rule("*", "*"))},
			from:     "", to: "",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-everything", Reason: "denied by deny-everything rule 1"},
		},

		// Degenerate inputs must not panic: nil entries and empty specs are skipped.
		{
			name:     "nil policy entry ignored",
			policies: []*klitev1.NetworkPolicy{nil, denyAtoC},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-a-to-c", Reason: "denied by deny-a-to-c rule 1"},
		},
		{
			name:     "policy without spec ignored",
			policies: []*klitev1.NetworkPolicy{{Meta: &klitev1.Meta{Name: "hollow"}}, denyAtoC},
			from:     "a", to: "c",
			want: policy.Decision{Allowed: false, MatchedPolicy: "deny-a-to-c", Reason: "denied by deny-a-to-c rule 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := policy.Evaluate(tt.policies, tt.from, tt.to)
			if got != tt.want {
				t.Errorf("Evaluate(%q, %q) = %+v, want %+v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestCompile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policies []*klitev1.NetworkPolicy
		want     []*klitev1.CompiledPolicy
	}{
		{
			name: "empty policy set",
		},
		{
			name: "flattens sorted by policy name then rule index",
			policies: []*klitev1.NetworkPolicy{
				pol("z-deny", deny, rule("a", "*", "b", "c"), rule("*", "d")),
				pol("a-allow", allow, rule("a", "b")),
			},
			want: []*klitev1.CompiledPolicy{
				{Action: allow, From: "a", To: "b", PolicyName: "a-allow"},
				{Action: deny, From: "a", To: "*", Except: []string{"b", "c"}, PolicyName: "z-deny"},
				{Action: deny, From: "*", To: "d", PolicyName: "z-deny"},
			},
		},
		{
			name:     "policy with no rules contributes nothing",
			policies: []*klitev1.NetworkPolicy{pol("empty-deny", deny)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := policy.Compile(tt.policies)
			if len(got) != len(tt.want) {
				t.Fatalf("Compile() returned %d entries, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if !proto.Equal(got[i], tt.want[i]) {
					t.Errorf("Compile()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCompileDoesNotAliasExcept(t *testing.T) {
	t.Parallel()
	src := pol("lockdown-a", deny, rule("a", "*", "b"))
	got := policy.Compile([]*klitev1.NetworkPolicy{src})
	got[0].Except[0] = "mutated"
	if src.Spec.Rules[0].Except[0] != "b" {
		t.Error("Compile() aliased the source except slice")
	}
}

// FuzzEvaluate holds the evaluator to three invariants on arbitrary names:
//  1. same inputs give the same decision regardless of policy order
//  2. a matching DENY rule forces a denial
//  3. every decision carries a reason
func FuzzEvaluate(f *testing.F) {
	policies := []*klitev1.NetworkPolicy{
		pol("deny-a-to-c", deny, rule("a", "c")),
		pol("lockdown-a", deny, rule("a", "*", "b")),
		pol("allow-only-a-to-b", allow, rule("a", "b")),
		pol("allow-broad", allow, rule("*", "*", "c")),
	}
	reversed := make([]*klitev1.NetworkPolicy, len(policies))
	for i, p := range policies {
		reversed[len(policies)-1-i] = p
	}

	f.Add("a", "c")
	f.Add("a", "b")
	f.Add("", "")
	f.Add("*", "*")
	f.Add("café", "日本")
	f.Fuzz(func(t *testing.T, from, to string) {
		got := policy.Evaluate(policies, from, to)
		if again := policy.Evaluate(policies, from, to); got != again {
			t.Errorf("Evaluate(%q, %q) unstable: %+v then %+v", from, to, got, again)
		}
		if flipped := policy.Evaluate(reversed, from, to); got != flipped {
			t.Errorf("Evaluate(%q, %q) depends on input order: %+v vs %+v", from, to, got, flipped)
		}
		if deniesNaively(policies, from, to) && got.Allowed {
			t.Errorf("Evaluate(%q, %q) allowed despite a matching DENY: %+v", from, to, got)
		}
		if got.Reason == "" {
			t.Errorf("Evaluate(%q, %q) returned an empty reason", from, to)
		}
	})
}

// deniesNaively is the fuzz oracle, a straight scan for any DENY rule
// matching from->to.
func deniesNaively(policies []*klitev1.NetworkPolicy, from, to string) bool {
	for _, p := range policies {
		if p.GetSpec().GetAction() != deny {
			continue
		}
		for _, r := range p.GetSpec().GetRules() {
			if denyRuleMatches(r, from, to) {
				return true
			}
		}
	}
	return false
}

func denyRuleMatches(r *klitev1.PolicyRule, from, to string) bool {
	if r.GetFrom() != "*" && r.GetFrom() != from {
		return false
	}
	if r.GetTo() == to {
		return true
	}
	return r.GetTo() == "*" && !slices.Contains(r.GetExcept(), to)
}
