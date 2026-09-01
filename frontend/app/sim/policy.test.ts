import { describe, expect, it } from 'bun:test'
import type { NetworkPolicy } from '@/api/types'
import { evaluate } from './policy'

function policy(
  name: string,
  action: 'ALLOW' | 'DENY',
  rules: NetworkPolicy['spec']['rules'],
): NetworkPolicy {
  return { apiVersion: 'klite/v1', kind: 'NetworkPolicy', metadata: { name }, spec: { action, rules } }
}

describe('istio-lite evaluation (ADR 0009)', () => {
  it('defaults to allow with no policies', () => {
    expect(evaluate([], 'a', 'b')).toEqual({ allowed: true, reason: 'default-allow' })
  })

  it('DENY beats a matching ALLOW', () => {
    const ps = [
      policy('allow-a-to-c', 'ALLOW', [{ from: 'a', to: 'c' }]),
      policy('deny-a-to-c', 'DENY', [{ from: 'a', to: 'c' }]),
    ]
    const v = evaluate(ps, 'a', 'c')
    expect(v.allowed).toBe(false)
    expect(v.matchedRule).toEqual({ policy: 'deny-a-to-c', ruleIndex: 0, action: 'DENY' })
  })

  it('ALLOW-flip: the first ALLOW targeting a service puts it in allowlist mode', () => {
    const ps = [policy('allow-only-a-to-b', 'ALLOW', [{ from: 'a', to: 'b' }])]
    expect(evaluate(ps, 'a', 'b').allowed).toBe(true)
    expect(evaluate(ps, 'c', 'b')).toEqual({ allowed: false, reason: 'no-allow-match' })
    // b itself is untargeted as a caller destination elsewhere: c stays open
    expect(evaluate(ps, 'b', 'c').reason).toBe('default-allow')
  })

  it('lockdown-a: deny a → * except [b]', () => {
    const ps = [policy('lockdown-a', 'DENY', [{ from: 'a', to: '*', except: ['b'] }])]
    expect(evaluate(ps, 'a', 'b').allowed).toBe(true)
    expect(evaluate(ps, 'a', 'c').allowed).toBe(false)
    expect(evaluate(ps, 'a', 'z').allowed).toBe(false)
    expect(evaluate(ps, 'c', 'b').allowed).toBe(true) // only a is locked down
  })

  it('wildcard from matches every caller', () => {
    const ps = [policy('deny-all-to-c', 'DENY', [{ from: '*', to: 'c' }])]
    expect(evaluate(ps, 'a', 'c').allowed).toBe(false)
    expect(evaluate(ps, 'b', 'c').allowed).toBe(false)
    expect(evaluate(ps, 'a', 'b').allowed).toBe(true)
  })

  it('verdicts name the policy and rule index', () => {
    const ps = [
      policy('multi', 'DENY', [
        { from: 'x', to: 'y' },
        { from: 'a', to: 'c' },
      ]),
    ]
    expect(evaluate(ps, 'a', 'c').matchedRule).toEqual({ policy: 'multi', ruleIndex: 1, action: 'DENY' })
  })
})
