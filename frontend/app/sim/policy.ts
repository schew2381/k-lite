// evaluate() implements the istio-lite semantics of ADR 0009 as one pure
// function. The traffic path, policyCheck, and the UI's simulator all import
// it, so a verdict shown anywhere is the data path's own.

import type { NetworkPolicy, PolicyVerdict, Verdict } from '@/api/types'

function matches(selector: string, name: string): boolean {
  return selector === '*' || selector === name
}

export function evaluate(rawPolicies: NetworkPolicy[], from: string, to: string): Verdict {
  // Sort by name like the Go evaluator, so matchedRule agrees when several
  // policies match the same pair.
  const policies = [...rawPolicies].sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
  // DENY phase: any matching DENY rule wins outright.
  for (const p of policies) {
    if (p.spec.action !== 'DENY') continue
    for (let i = 0; i < p.spec.rules.length; i++) {
      const r = p.spec.rules[i]
      if (matches(r.from, from) && matches(r.to, to) && !r.except?.includes(to)) {
        return {
          allowed: false,
          reason: 'deny-rule',
          matchedRule: { policy: p.metadata.name, ruleIndex: i, action: 'DENY' },
        }
      }
    }
  }

  // ALLOW phase: a Service no ALLOW rule targets accepts everyone. The first
  // ALLOW that targets it flips it to allowlist mode.
  let targeted = false
  for (const p of policies) {
    if (p.spec.action !== 'ALLOW') continue
    for (let i = 0; i < p.spec.rules.length; i++) {
      const r = p.spec.rules[i]
      if (!matches(r.to, to) || r.except?.includes(to)) continue
      targeted = true
      if (matches(r.from, from)) {
        return {
          allowed: true,
          reason: 'allow-rule',
          matchedRule: { policy: p.metadata.name, ruleIndex: i, action: 'ALLOW' },
        }
      }
    }
  }

  if (targeted) return { allowed: false, reason: 'no-allow-match' }
  return { allowed: true, reason: 'default-allow' }
}

// toPolicyVerdict renders the structured verdict in the client-seam shape,
// wording the reason exactly as internal/policy/policy.go does, so the mock
// and the real facade answer with identical sentences.
export function toPolicyVerdict(v: Verdict, from: string, to: string): PolicyVerdict {
  const rule = v.matchedRule
  let reason: string
  switch (v.reason) {
    case 'deny-rule':
      reason = `denied by ${rule?.policy} rule ${(rule?.ruleIndex ?? 0) + 1}`
      break
    case 'allow-rule':
      reason = `allowed by ${rule?.policy} rule ${(rule?.ruleIndex ?? 0) + 1}`
      break
    case 'default-allow':
      reason = `no ALLOW targets ${to}, default allow`
      break
    case 'no-allow-match':
      reason = `${to} is allowlist-mode and no ALLOW admits ${from}`
      break
  }
  return {
    available: true,
    allowed: v.allowed,
    matchedPolicy: rule?.policy,
    reason,
  }
}
