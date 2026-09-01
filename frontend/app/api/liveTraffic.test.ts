import { describe, expect, it } from 'bun:test'
import { Cluster } from '@/sim/cluster'
import { seedObjects } from '@/sim/seed'
import { ClusterStore } from '@/store/store'
import { enrichTrafficDelta } from './liveTraffic'

function liveSnapshot() {
  const c = new Cluster(seedObjects())
  for (let t = 0; t < 6000; t += 100) c.advance(100)
  const store = new ClusterStore()
  c.subscribe(store.applyEvent)
  return store.getSnapshot()
}

const base = { unixMs: 1_000, node: 'node-1', count: 1 }

describe('enrichTrafficDelta', () => {
  it('maps an instance IP to the instance behind it', () => {
    const s = liveSnapshot()
    const inst = Object.values(s.instances).find((i) => i.status.instanceIp)
    if (!inst) throw new Error('seed produced no instance with an IP')
    const svc = inst.spec.workload
    const [ev] = enrichTrafficDelta(
      { ...base, service: svc, address: inst.status.instanceIp as string, port: 80 },
      s,
    )
    expect(ev.toInstance).toBe(inst.metadata.name)
    expect(ev.toService).toBe(svc)
    expect(ev.viaNode).toBe('node-1')
    expect(ev.fromInstance).toBe('')
    expect(ev.verdict).toBe('allowed')
  })

  it('maps an ingress port on a machine address to the published instance', () => {
    const s = liveSnapshot()
    const alloc = Object.values(s.ingressAllocations)[0]
    if (!alloc) throw new Error('seed produced no ingress allocations')
    const [ev] = enrichTrafficDelta(
      { ...base, service: alloc.spec.service, address: '198.51.100.9', port: alloc.spec.port },
      s,
    )
    expect(ev.toInstance).toBe(alloc.spec.instance)
  })

  it('drops deltas for services the snapshot does not know', () => {
    const s = liveSnapshot()
    expect(enrichTrafficDelta({ ...base, service: 'ghost', address: '10.0.0.1', port: 80 }, s)).toEqual(
      [],
    )
  })

  it('turns a denied delta into a policy denial with its RBAC phase', () => {
    const s = liveSnapshot()
    const svc = Object.keys(s.services)[0]
    const [ev] = enrichTrafficDelta(
      { ...base, service: svc, count: 1, verdict: 'denied', rbacPhase: 'deny' },
      s,
    )
    expect(ev.verdict).toBe('denied')
    expect(ev.reason).toBe('policy')
    expect(ev.rbacPhase).toBe('deny')
    expect(ev.toInstance).toBeUndefined()
  })

  it('starts the story at the caller chip when the kdns ring named it', () => {
    const s = liveSnapshot()
    const inst = Object.values(s.instances).find((i) => i.status.instanceIp)
    if (!inst) throw new Error('seed produced no instance with an IP')
    const target = Object.keys(s.services).find((n) => n !== inst.spec.workload) as string
    const [ev] = enrichTrafficDelta(
      { ...base, service: target, address: '10.9.9.9', port: 80, caller: inst.status.instanceIp },
      s,
    )
    expect(ev.fromInstance).toBe(inst.metadata.name)
    expect(ev.fromService).toBe(inst.spec.workload)
  })

  it('caps one delta row at a handful of events', () => {
    const s = liveSnapshot()
    const svc = Object.keys(s.services)[0]
    const events = enrichTrafficDelta(
      { ...base, count: 9, service: svc, address: '10.9.9.9', port: 80 },
      s,
    )
    expect(events.length).toBe(4)
    expect(new Set(events.map((e) => e.id)).size).toBe(4)
  })
})
