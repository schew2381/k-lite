// buildTrace tells a local story in seven steps and a remote one in nine.
// Three hops replace the single relay: the internet leg, the DNAT hop into
// the target's infra pod, and the raw local hand-off.

import { describe, expect, it } from 'bun:test'
import type { TrafficEvent } from '@/api/types'
import { Cluster } from '@/sim/cluster'
import { seedObjects } from '@/sim/seed'
import { buildTrace } from './trace'

function settle(c: Cluster, ms: number) {
  for (let t = 0; t < ms; t += 100) c.advance(100)
}

function eventFor(c: Cluster, remote: boolean): TrafficEvent {
  const instances = c.list('Instance').filter((o) => o.kind === 'Instance')
  for (const caller of instances) {
    if (!caller.spec.node) continue
    const target = instances.find(
      (i) => i.spec.workload !== caller.spec.workload && (i.spec.node !== caller.spec.node) === remote,
    )
    if (!target) continue
    return {
      id: 't1',
      ts: 0,
      fromInstance: caller.metadata.name,
      fromService: caller.spec.workload,
      toService: target.spec.workload,
      viaNode: caller.spec.node,
      verdict: 'allowed',
      toInstance: target.metadata.name,
      latencyMs: 30,
    }
  }
  throw new Error('no pair with the wanted locality')
}

function snapshotFromCluster(c: Cluster) {
  const maps = {
    workloads: {},
    services: {},
    nodes: {},
    instances: {},
    policies: {},
    vipAllocations: {},
    ingressAllocations: {},
  } as Record<string, Record<string, unknown>>
  const keyOf = {
    Workload: 'workloads',
    Service: 'services',
    Node: 'nodes',
    Instance: 'instances',
    NetworkPolicy: 'policies',
    VIPAllocation: 'vipAllocations',
    IngressAllocation: 'ingressAllocations',
  } as const
  for (const kind of Object.keys(keyOf) as (keyof typeof keyOf)[]) {
    for (const obj of c.list(kind)) maps[keyOf[kind]][obj.metadata.name] = obj
  }
  return { rev: 1, synced: true, ...maps } as unknown as Parameters<typeof buildTrace>[1]
}

describe('buildTrace locality', () => {
  it('keeps the seven-step story when the pick is on the caller node', () => {
    const c = new Cluster(seedObjects())
    settle(c, 5000)
    const s = snapshotFromCluster(c)
    const trace = buildTrace(eventFor(c, false), s)
    expect(trace.steps).toHaveLength(7)
    expect(trace.steps.some((st) => st.at === 'targetInfra')).toBe(false)
  })

  it('tells the internet story for a remote pick: mTLS leg, DNAT hop, raw hand-off', () => {
    const c = new Cluster(seedObjects())
    settle(c, 5000)
    const s = snapshotFromCluster(c)
    const trace = buildTrace(eventFor(c, true), s)
    expect(trace.steps).toHaveLength(9)
    const shorts = trace.steps.map((st) => st.short).join(' | ')
    expect(shorts).toContain('remote')
    expect(shorts).toMatch(/mTLS → 198\.51\.100\.\d+:\d+/)
    expect(shorts).toMatch(/DNAT :\d+ → envoy/)
    expect(trace.targetNode).not.toBe(trace.event.viaNode)
    const infraSteps = trace.steps.filter((st) => st.at === 'targetInfra')
    expect(infraSteps).toHaveLength(2)
  })
})
