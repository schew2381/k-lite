import { describe, expect, it } from 'bun:test'
import { Cluster } from '@/sim/cluster'
import { seedObjects } from '@/sim/seed'
import { ClusterStore } from '@/store/store'
import { computeLayout } from './layout'

function snapshotAfter(ms: number) {
  const c = new Cluster(seedObjects())
  for (let t = 0; t < ms; t += 100) c.advance(100)
  const store = new ClusterStore()
  c.subscribe(store.applyEvent)
  return store.getSnapshot()
}

describe('computeLayout', () => {
  it('is deterministic for the same snapshot and width', () => {
    const snap = snapshotAfter(4000)
    expect(computeLayout(snap, 1200)).toEqual(computeLayout(snap, 1200))
  })

  it('gives every node, service, infra pod, and instance an anchor', () => {
    const snap = snapshotAfter(4000)
    const layout = computeLayout(snap, 1200)
    for (const name of Object.keys(snap.services)) {
      expect(layout.anchors[`service:${name}`]).toBeDefined()
    }
    for (const name of Object.keys(snap.nodes)) {
      for (const part of ['infra', 'kdns', 'lds', 'rbac', 'eds'] as const) {
        expect(layout.anchors[`${part}:${name}`]).toBeDefined()
      }
      // every sub-box nests inside its infra pod box
      const infra = layout.nodes[name].infra
      for (const sub of [infra.kdns, infra.envoy, infra.lds, infra.rbac, infra.eds]) {
        expect(sub.y).toBeGreaterThanOrEqual(infra.box.y)
        expect(sub.y + sub.h).toBeLessThanOrEqual(infra.box.y + infra.box.h)
        expect(sub.x).toBeGreaterThanOrEqual(infra.box.x)
        expect(sub.x + sub.w).toBeLessThanOrEqual(infra.box.x + infra.box.w)
      }
    }
    for (const name of Object.keys(snap.instances)) {
      expect(layout.anchors[`instance:${name}`]).toBeDefined()
    }
  })

  it('reflows to fewer columns at narrow widths without losing cards', () => {
    const snap = snapshotAfter(4000)
    const wide = computeLayout(snap, 1400)
    const narrow = computeLayout(snap, 500)
    expect(Object.keys(narrow.nodes)).toEqual(Object.keys(wide.nodes))
    expect(narrow.height).toBeGreaterThan(wide.height)
    for (const nl of Object.values(narrow.nodes)) {
      expect(nl.card.x + nl.card.w).toBeLessThanOrEqual(500)
    }
  })

  it('grows a card with its instance count', () => {
    const snap = snapshotAfter(4000)
    const layout = computeLayout(snap, 360) // one chip per row at this width
    const counts = new Map<string, number>()
    for (const inst of Object.values(snap.instances)) {
      counts.set(inst.spec.node ?? '', (counts.get(inst.spec.node ?? '') ?? 0) + 1)
    }
    const [big] = [...counts.entries()].sort((a, b) => b[1] - a[1])
    const [small] = [...counts.entries()].sort((a, b) => a[1] - b[1])
    if (big[1] !== small[1]) {
      expect(layout.nodes[big[0]].card.h).toBeGreaterThan(layout.nodes[small[0]].card.h)
    }
  })

  it('drops anchors for deleted instances', () => {
    const c = new Cluster(seedObjects())
    for (let t = 0; t < 4000; t += 100) c.advance(100)
    const store = new ClusterStore()
    c.subscribe(store.applyEvent)
    const before = computeLayout(store.getSnapshot(), 1200)
    const victim = Object.keys(store.getSnapshot().instances)[0]
    expect(before.anchors[`instance:${victim}`]).toBeDefined()

    c.remove('Workload', store.getSnapshot().instances[victim].spec.workload)
    for (let t = 0; t < 10000; t += 100) c.advance(100)
    const after = computeLayout(store.getSnapshot(), 1200)
    expect(after.anchors[`instance:${victim}`]).toBeUndefined()
  })
})
