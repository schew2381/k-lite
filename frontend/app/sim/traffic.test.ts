import { describe, expect, it } from 'bun:test'
import type { TrafficEvent, Workload } from '@/api/types'
import { Cluster } from './cluster'
import { toPolicyVerdict } from './policy'
import { seedObjects } from './seed'
import { DEFAULT_TIMINGS } from './timings'

// tests run traffic at one call per second. Demo pacing is a UI concern
const DENSE = { ...DEFAULT_TIMINGS, trafficPeriodMs: 1000 }

function settle(c: Cluster, ms: number, step = 100) {
  for (let t = 0; t < ms; t += step) c.advance(step)
}

function converged(): { c: Cluster; events: TrafficEvent[] } {
  const c = new Cluster(seedObjects(), DENSE)
  settle(c, 4000)
  const events: TrafficEvent[] = []
  c.subscribeTraffic((e) => events.push(e))
  return { c, events }
}

const DENY_A_TO_C = {
  apiVersion: 'klite/v1' as const,
  kind: 'NetworkPolicy' as const,
  metadata: { name: 'deny-a-to-c' },
  spec: { action: 'DENY' as const, rules: [{ from: 'a', to: 'c' }] },
}

describe('random traffic generation', () => {
  it('never calls the caller’s own service, and every pair shows up eventually', () => {
    const { c, events } = converged()
    settle(c, 90000) // sim time is cheap, and the rarest pair needs a long window
    expect(events.length).toBeGreaterThan(20)
    for (const e of events) expect(e.fromService).not.toBe(e.toService)
    const pairs = new Set(events.map((e) => `${e.fromService}→${e.toService}`))
    for (const want of ['a→b', 'a→c', 'b→a', 'b→c', 'c→a', 'c→b']) {
      expect(pairs.has(want)).toBe(true)
    }
  })

  it('carries the enforcing node and, when allowed, the instance the call landed on', () => {
    const { c, events } = converged()
    settle(c, 20000)
    for (const e of events) {
      expect(e.viaNode.startsWith('node-')).toBe(true)
      if (e.verdict === 'allowed') {
        expect(e.toInstance).toBeDefined()
        expect(e.latencyMs).toBeGreaterThan(0)
      }
    }
  })

  it('round-robins replicas per (node, service) — the per-node Envoy LB state', () => {
    const { c, events } = converged()
    settle(c, 120000)
    const series = events
      .filter((e) => e.viaNode === 'node-1' && e.toService === 'b' && e.toInstance)
      .map((e) => e.toInstance)
    expect(new Set(series).size).toBe(2)
    for (let i = 1; i < series.length; i++) expect(series[i]).not.toBe(series[i - 1])
  })

  it('a policy flips verdicts at call time, and back on delete', () => {
    const { c, events } = converged()
    c.applyObjects([DENY_A_TO_C])
    settle(c, 90000)
    const aToC = events.filter((e) => e.fromService === 'a' && e.toService === 'c')
    expect(aToC.length).toBeGreaterThan(0)
    expect(aToC.every((e) => e.verdict === 'denied' && e.reason === 'policy')).toBe(true)
    expect(aToC[0].matchedRule).toEqual({ policy: 'deny-a-to-c', ruleIndex: 0, action: 'DENY' })
    const aToB = events.filter((e) => e.fromService === 'a' && e.toService === 'b')
    expect(aToB.every((e) => e.verdict === 'allowed')).toBe(true)

    // a has one instance rolling a ~37% chance of dialing c each second, so
    // give the seeded dice a wide window
    events.length = 0
    c.remove('NetworkPolicy', 'deny-a-to-c')
    settle(c, 90000)
    const after = events.filter((e) => e.fromService === 'a' && e.toService === 'c')
    expect(after.length).toBeGreaterThan(0)
    expect(after.every((e) => e.verdict === 'allowed')).toBe(true)
  })

  it('policyCheck and the traffic path speak with one voice', () => {
    const { c, events } = converged()
    c.applyObjects([DENY_A_TO_C])
    const check = c.policyCheck('a', 'c')
    settle(c, 90000)
    const observed = events.find((e) => e.fromService === 'a' && e.toService === 'c')
    expect(check.allowed).toBe(false)
    expect(observed?.verdict).toBe('denied')
    expect(observed?.matchedRule).toEqual(check.matchedRule)
  })

  it('reports no-endpoints distinctly when a service has nothing READY', () => {
    const { c, events } = converged()
    const wl = c.get('Workload', 'c') as Workload
    wl.spec.replicas = 0
    c.applyObjects([wl])
    settle(c, 40000) // c drains to nothing while callers keep dialing the name
    const noEp = events.filter((e) => e.toService === 'c' && e.reason === 'no-endpoints')
    expect(noEp.length).toBeGreaterThan(0)
    expect(noEp.every((e) => !e.matchedRule)).toBe(true) // not a policy denial
  })

  it('logs narrate both sides of a call, naming the policy that blocks one', () => {
    const { c } = converged()
    settle(c, 90000)
    const a1lines: string[] = []
    c.logs.subscribe('a-1', (l) => a1lines.push(l.line))
    expect(a1lines.some((l) => l.includes('=> Hostname:'))).toBe(true)
    expect(a1lines.some((l) => l.includes('GET / from'))).toBe(true)
    c.applyObjects([DENY_A_TO_C])
    settle(c, 90000)
    expect(a1lines.some((l) => l.includes('c => blocked by deny-a-to-c'))).toBe(true)
  })
})

describe('vip allocations', () => {
  it('materializes one per (service, node), heals deletion, and releases on service delete', () => {
    const c = new Cluster(seedObjects(), DENSE)
    settle(c, 4000)
    expect(c.list('VIPAllocation').length).toBe(9) // 3 services × 3 nodes
    const before = c.get('VIPAllocation', 'b.node-2')
    expect(before?.kind).toBe('VIPAllocation')

    // hand-delete a server-materialized object and the reconciler puts it back
    c.remove('VIPAllocation', 'b.node-2')
    settle(c, 2000)
    expect(c.get('VIPAllocation', 'b.node-2')).not.toBeNull()

    c.remove('Service', 'b')
    settle(c, 2000)
    const leaked = c
      .list('VIPAllocation')
      .filter((a) => a.kind === 'VIPAllocation' && a.spec.service === 'b')
    expect(leaked).toEqual([])
  })

  it('the traced VIP comes from the allocation object', () => {
    const c = new Cluster(seedObjects(), DENSE)
    settle(c, 4000)
    const alloc = c.get('VIPAllocation', 'c.node-1')
    expect(alloc?.kind === 'VIPAllocation' && alloc.spec.vip).toMatch(
      /^10\.44\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\./,
    )
    expect(c.vipFor('c', 'node-1')).toBe(alloc?.kind === 'VIPAllocation' ? alloc.spec.vip : '')
  })
})

describe('policy verdict phrasing', () => {
  it("matches internal/policy's sentences in every branch", () => {
    expect(toPolicyVerdict({ allowed: true, reason: 'default-allow' }, 'a', 'b')).toEqual({
      available: true,
      allowed: true,
      matchedPolicy: undefined,
      reason: 'no ALLOW targets b, default allow',
    })
    expect(
      toPolicyVerdict(
        {
          allowed: false,
          reason: 'deny-rule',
          matchedRule: { policy: 'lockdown-a', ruleIndex: 0, action: 'DENY' },
        },
        'b',
        'a',
      ).reason,
    ).toBe('denied by lockdown-a rule 1')
    expect(
      toPolicyVerdict(
        {
          allowed: true,
          reason: 'allow-rule',
          matchedRule: { policy: 'vip-only', ruleIndex: 1, action: 'ALLOW' },
        },
        'a',
        'b',
      ).reason,
    ).toBe('allowed by vip-only rule 2')
    expect(toPolicyVerdict({ allowed: false, reason: 'no-allow-match' }, 'c', 'b').reason).toBe(
      'b is allowlist-mode and no ALLOW admits c',
    )
  })
})
