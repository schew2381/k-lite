import { describe, expect, it } from 'bun:test'
import { endpointStateOf, type Instance, type TrafficEvent } from '@/api/types'
import { Cluster } from './cluster'
import { seedObjects } from './seed'
import { DEFAULT_TIMINGS } from './timings'

// drain assertions need dense traffic, and one call per second keeps them sharp
const DENSE = { ...DEFAULT_TIMINGS, trafficPeriodMs: 1000 }

function settle(c: Cluster, ms: number, step = 100) {
  for (let t = 0; t < ms; t += step) c.advance(step)
}

describe('surge-first drain (ADR 0010)', () => {
  it('creates the replacement and waits for Ready before the old endpoint turns DRAINING', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const events: { phase: string; name: string; endpoint?: string }[] = []
    c.subscribe((e) => {
      if (e.kind === 'Instance' && e.object) {
        const i = e.object as Instance
        events.push({ phase: i.status.phase, name: i.metadata.name, endpoint: endpointStateOf(i) })
      }
    })

    const preexisting = new Set((c.list('Instance') as Instance[]).map((i) => i.metadata.name))
    const target = (c.list('Instance') as Instance[])[0].spec.node as string
    const evacuees = (c.list('Instance') as Instance[])
      .filter((i) => i.spec.node === target)
      .map((i) => i.metadata.name)
    c.drainNode(target)
    settle(c, 14000)

    // every evacuee eventually turned DRAINING…
    for (const name of evacuees) {
      const drainingAt = events.findIndex((e) => e.name === name && e.endpoint === 'DRAINING')
      expect(drainingAt).toBeGreaterThan(-1)
      // …and a Ready replacement of the same workload appeared before it turned DRAINING
      const wl = name.slice(0, name.lastIndexOf('-'))
      const replacementReadyAt = events.findIndex(
        (e) => e.phase === 'Ready' && e.name.startsWith(`${wl}-`) && !evacuees.includes(e.name),
      )
      expect(replacementReadyAt).toBeGreaterThan(-1)
      expect(replacementReadyAt).toBeLessThan(drainingAt)
    }

    // the drained node is empty and cordoned, and capacity is back to full
    const left = c.list('Instance') as Instance[]
    expect(left.filter((i) => i.spec.node === target).length).toBe(0)
    expect(left.filter((i) => i.status.phase === 'Ready').length).toBe(5)

    // exactly one replacement per evacuee — a draining instance must never
    // count toward replicas, or the reconciler churns replacements forever
    const uniqueNew = new Set(events.map((e) => e.name).filter((n) => !preexisting.has(n)))
    expect(uniqueNew.size).toBe(evacuees.length)
  })

  it('records zero failed calls during a full node drain', () => {
    const c = new Cluster(seedObjects(), DENSE)
    settle(c, 4000)
    const failures: TrafficEvent[] = []
    c.subscribeTraffic((e) => {
      if (e.verdict === 'denied') failures.push(e)
    })
    const target = (c.list('Instance') as Instance[]).find((i) => i.spec.workload === 'b')?.spec
      .node as string
    c.drainNode(target)
    settle(c, 16000)
    expect(failures).toEqual([])
  })

  it('removing a node deletes it once drained, and its VIPs go with it', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    c.remove('Node', 'node-3')
    settle(c, 16000)
    expect(c.get('Node', 'node-3')).toBeNull()
    const svcB = c.get('Service', 'b')
    expect(
      (svcB as { status?: { vips: Record<string, string> } }).status?.vips['node-3'],
    ).toBeUndefined()
  })

  it('a watch consumer also sees the node stay deleted — no event after DELETED resurrects it', async () => {
    // regression: the sweep once emitted a MODIFIED for the node right after
    // its DELETED, so the cluster map looked right while every watch replay
    // (the UI store) brought the node back
    const { ClusterStore } = await import('@/store/store')
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const store = new ClusterStore()
    c.subscribe(store.applyEvent)
    c.remove('Node', 'node-3')
    settle(c, 20000)
    expect(store.getSnapshot().nodes['node-3']).toBeUndefined()
    expect(Object.keys(store.getSnapshot().nodes).sort()).toEqual(['node-1', 'node-2'])
  })

  it('never routes to a DRAINING endpoint, checked at hit time', () => {
    const c = new Cluster(seedObjects(), DENSE)
    settle(c, 4000)
    // live view of every instance's endpoint state, maintained from the watch
    const states = new Map<string, string | undefined>()
    c.subscribe((e) => {
      if (e.kind !== 'Instance' || !e.object) return
      const i = e.object as Instance
      if (e.type === 'DELETED') states.delete(i.metadata.name)
      else states.set(i.metadata.name, endpointStateOf(i))
    })
    const violations: string[] = []
    let hits = 0
    c.subscribeTraffic((e) => {
      if (!e.toInstance) return
      hits++
      if (states.get(e.toInstance) !== 'READY') violations.push(e.toInstance)
    })
    const target = (c.list('Instance') as Instance[]).find((i) => i.spec.workload === 'b')?.spec
      .node as string
    c.drainNode(target)
    settle(c, 16000)
    expect(hits).toBeGreaterThan(5)
    expect(violations).toEqual([])
  })
})
