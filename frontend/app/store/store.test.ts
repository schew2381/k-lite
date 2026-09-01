import { describe, expect, it } from 'bun:test'
import type { NodeObj, WatchEvent } from '@/api/types'
import { endpointsOf } from './selectors'
import { ClusterStore } from './store'

function nodeEvent(type: 'ADDED' | 'MODIFIED' | 'DELETED', name: string, rev: number): WatchEvent {
  const node: NodeObj = {
    apiVersion: 'klite/v1',
    kind: 'Node',
    metadata: { name },
    spec: { maxInstances: 32 },
    status: { phase: 'Ready', instanceCount: 0 },
  }
  return { type, rev, kind: 'Node', object: node }
}

const flushMicrotasks = () => new Promise<void>((r) => queueMicrotask(r))

describe('ClusterStore', () => {
  it('replays ADDED / MODIFIED / DELETED into the snapshot', () => {
    const store = new ClusterStore()
    store.applyEvent(nodeEvent('ADDED', 'node-1', 1))
    store.applyEvent(nodeEvent('ADDED', 'node-2', 2))
    store.applyEvent(nodeEvent('DELETED', 'node-1', 3))
    const snap = store.getSnapshot()
    expect(Object.keys(snap.nodes)).toEqual(['node-2'])
    expect(snap.rev).toBe(3)
  })

  it('SYNC flips synced; RESET clears everything', () => {
    const store = new ClusterStore()
    store.applyEvent(nodeEvent('ADDED', 'node-1', 1))
    expect(store.getSnapshot().synced).toBe(false)
    store.applyEvent({ type: 'SYNC', rev: 1 })
    expect(store.getSnapshot().synced).toBe(true)
    store.applyEvent({ type: 'RESET', rev: 0 })
    expect(store.getSnapshot().synced).toBe(false)
    expect(Object.keys(store.getSnapshot().nodes)).toEqual([])
  })

  it('collapses an event burst into one notification', async () => {
    const store = new ClusterStore()
    let notified = 0
    store.subscribe(() => notified++)
    for (let i = 1; i <= 20; i++) store.applyEvent(nodeEvent('ADDED', `node-${i}`, i))
    await flushMicrotasks()
    expect(notified).toBe(1)
    expect(Object.keys(store.getSnapshot().nodes).length).toBe(20)
  })

  it('snapshot identity is stable between events (useSyncExternalStore-safe)', () => {
    const store = new ClusterStore()
    store.applyEvent(nodeEvent('ADDED', 'node-1', 1))
    const a = store.getSnapshot()
    const b = store.getSnapshot()
    expect(a).toBe(b)
    store.applyEvent(nodeEvent('MODIFIED', 'node-1', 2))
    expect(store.getSnapshot()).not.toBe(a)
  })

  it('endpointsOf splits READY from DRAINING and ignores the rest', () => {
    const store = new ClusterStore()
    const mk = (name: string, phase: 'Ready' | 'Draining' | 'Running'): WatchEvent => ({
      type: 'ADDED',
      rev: 1,
      kind: 'Instance',
      object: {
        apiVersion: 'klite/v1',
        kind: 'Instance',
        metadata: { name, labels: { app: 'b' } },
        spec: { workload: 'b', node: 'node-1', container: { name: 'web', image: 'img' } },
        status: { phase, restarts: 0 },
      },
    })
    store.applyEvent(mk('b-1', 'Ready'))
    store.applyEvent(mk('b-2', 'Draining'))
    store.applyEvent(mk('b-3', 'Running'))
    const svc = {
      apiVersion: 'klite/v1' as const,
      kind: 'Service' as const,
      metadata: { name: 'b' },
      spec: { selector: { app: 'b' }, port: 8080, targetPort: 80 },
    }
    const { ready, draining } = endpointsOf(store.getSnapshot(), svc)
    expect(ready.map((i) => i.metadata.name)).toEqual(['b-1'])
    expect(draining.map((i) => i.metadata.name)).toEqual(['b-2'])
  })
})
