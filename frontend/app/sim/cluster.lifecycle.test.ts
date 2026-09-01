import { describe, expect, it } from 'bun:test'
import type { Instance, NodeObj, Workload } from '@/api/types'
import { Cluster } from './cluster'
import { seedObjects } from './seed'

function settle(c: Cluster, ms: number, step = 100) {
  for (let t = 0; t < ms; t += step) c.advance(step)
}

function instances(c: Cluster): Instance[] {
  return c.list('Instance') as Instance[]
}

describe('instance lifecycle', () => {
  it('converges the seed: Pending → Running → Ready with the probe delay honored', () => {
    const c = new Cluster(seedObjects())
    c.advance(100)
    // declare-then-converge: records exist before anything "runs"
    const early = instances(c)
    expect(early.length).toBe(5) // a×1, b×2, c×2
    expect(early.every((i) => i.status.phase === 'Pending')).toBe(true)

    settle(c, 1500) // infra up (~1s) + container start
    const running = instances(c).find((i) => i.spec.workload === 'a')
    expect(['Running', 'Ready']).toContain(running?.status.phase ?? 'missing')

    settle(c, 2500)
    const all = instances(c)
    expect(all.every((i) => i.status.phase === 'Ready')).toBe(true)
  })

  it('a (no probe) turns Ready while the probed b is still Running', () => {
    const c = new Cluster(seedObjects())
    let guard = 0
    while (!instances(c).some((i) => i.spec.workload === 'a' && i.status.phase === 'Ready')) {
      c.advance(100)
      if (++guard > 300) throw new Error('a never became Ready')
    }
    const bPhases = instances(c)
      .filter((i) => i.spec.workload === 'b')
      .map((i) => i.status.phase)
    expect(bPhases.length).toBeGreaterThan(0)
    expect(bPhases.every((phase) => phase !== 'Ready')).toBe(true)
  })

  it('spreads 5 instances 2/2/1 across 3 nodes', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const byNode = new Map<string, number>()
    for (const i of instances(c))
      byNode.set(i.spec.node ?? '?', (byNode.get(i.spec.node ?? '?') ?? 0) + 1)
    expect([...byNode.values()].sort()).toEqual([1, 2, 2])
  })

  it('kill → Failed → backoff restart → Ready with an honest count; backoff doubles', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const b1 = instances(c).find((i) => i.spec.workload === 'b') as Instance
    c.killInstance(b1.metadata.name)
    expect((c.get('Instance', b1.metadata.name) as Instance).status.phase).toBe('Failed')

    settle(c, 600) // first backoff is 500ms
    expect((c.get('Instance', b1.metadata.name) as Instance).status.phase).toBe('Running')
    settle(c, 1600) // probe
    const after = c.get('Instance', b1.metadata.name) as Instance
    expect(after.status.phase).toBe('Ready')
    expect(after.status.restarts).toBe(1)

    c.killInstance(b1.metadata.name)
    settle(c, 600) // second backoff is 1000ms — not up yet
    expect((c.get('Instance', b1.metadata.name) as Instance).status.phase).toBe('Failed')
    settle(c, 500)
    expect((c.get('Instance', b1.metadata.name) as Instance).status.phase).toBe('Running')
  })

  it('scale up lands on the emptiest node; scale down drains the newest', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const wl = c.get('Workload', 'b') as Workload
    wl.spec.replicas = 3
    c.applyObjects([wl])
    settle(c, 3000)
    const bs = instances(c).filter((i) => i.spec.workload === 'b')
    expect(bs.length).toBe(3)
    expect(bs.every((i) => i.status.phase === 'Ready')).toBe(true)

    wl.spec.replicas = 1
    c.applyObjects([wl])
    c.advance(100)
    const draining = instances(c).filter((i) => i.spec.workload === 'b' && i.status.phase === 'Draining')
    expect(draining.length).toBe(2)
    // newest first: b-3 must be among the victims
    expect(draining.map((i) => i.metadata.name)).toContain('b-3')

    settle(c, 8000) // drainTimeout + terminationGrace
    const left = instances(c).filter((i) => i.spec.workload === 'b')
    expect(left.length).toBe(1)
    expect(left[0].status.phase).toBe('Ready')
  })

  it('agent kill → NotReady after missed heartbeats → replacements after the grace', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const victim = instances(c)[0].spec.node as string
    const onVictim = instances(c)
      .filter((i) => i.spec.node === victim)
      .map((i) => i.metadata.name)
    expect(onVictim.length).toBeGreaterThan(0)

    c.killNodeAgent(victim)
    settle(c, 5600)
    expect((c.get('Node', victim) as NodeObj).status?.phase).toBe('NotReady')
    for (const name of onVictim) {
      expect((c.get('Instance', name) as Instance).status.message).toBe('node-lost')
    }

    settle(c, 8600) // reschedule grace
    for (const name of onVictim) expect(c.get('Instance', name)).toBeNull()
    settle(c, 3000)
    const healthy = instances(c).filter((i) => i.status.phase === 'Ready')
    expect(healthy.length).toBe(5) // full strength again, elsewhere
    expect(healthy.every((i) => i.spec.node !== victim)).toBe(true)
  })

  it('a revived agent brings its instances back without a reschedule', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const victim = instances(c)[0].spec.node as string
    c.killNodeAgent(victim)
    settle(c, 5600)
    c.reviveNodeAgent(victim)
    settle(c, 3000)
    expect((c.get('Node', victim) as NodeObj).status?.phase).toBe('Ready')
    expect(instances(c).filter((i) => i.status.phase === 'Ready').length).toBe(5)
  })
})
