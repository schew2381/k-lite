// evaluate() implements the istio-lite semantics of ADR 0009 as one pure
// function. The traffic path, policyCheck, and the UI's simulator all import
// it, so a verdict shown anywhere is the data path's own.

import type { NetworkPolicy, Verdict } from '@/api/types'

function matches(selector: string, name: string): boolean {
  return selector === '*' || selector === name
}

export function evaluate(policies: NetworkPolicy[], from: string, to: string): Verdict {
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
