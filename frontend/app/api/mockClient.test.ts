import { describe, expect, it } from 'bun:test'
import { Cluster } from '@/sim/cluster'
import { seedObjects } from '@/sim/seed'
import { DEFAULT_TIMINGS } from '@/sim/timings'

const DENSE = { ...DEFAULT_TIMINGS, trafficPeriodMs: 1000 }

import { parseApplyYaml } from './schemas'
import type { Instance, TrafficEvent, WatchEvent, Workload } from './types'

// The mock client is a thin adapter over Cluster + parseApplyYaml, so these tests
// cover the adapter's contract pieces directly, without the browser timer.

function settle(c: Cluster, ms: number, step = 100) {
  for (let t = 0; t < ms; t += step) c.advance(step)
}

describe('watch contract', () => {
  it('replays the full state as ADDED events, then SYNC, then goes live', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const events: WatchEvent[] = []
    c.subscribe((e) => events.push(e))

    const syncAt = events.findIndex((e) => e.type === 'SYNC')
    expect(syncAt).toBeGreaterThan(0)
    expect(events.slice(0, syncAt).every((e) => e.type === 'ADDED')).toBe(true)
    const replayed = events.slice(0, syncAt).length
    expect(replayed).toBe(
      c.list('Node').length +
        c.list('Workload').length +
        c.list('Service').length +
        c.list('Instance').length,
    )

    const before = events.length
    c.killInstance((c.list('Instance') as Instance[])[0].metadata.name)
    expect(events.length).toBeGreaterThan(before)
    expect(events[events.length - 1].type).toBe('MODIFIED')
  })
})

describe('apply through YAML', () => {
  it('round-trips a parsed document into cluster state', () => {
    const c = new Cluster(seedObjects())
    settle(c, 4000)
    const { objects, errors } = parseApplyYaml(`
apiVersion: klite/v1
kind: Workload
metadata:
  name: b
  labels: { app: b }
spec:
  replicas: 3
  template:
    labels: { app: b }
    containers:
      - name: web
        image: traefik/whoami:v1.10
        readinessProbe: { tcpPort: 80 }
`)
    expect(errors).toEqual([])
    const results = c.applyObjects(objects)
    expect(results[0]).toEqual({ kind: 'Workload', name: 'b', action: 'updated' })
    settle(c, 4000)
    expect((c.list('Instance') as Instance[]).filter((i) => i.spec.workload === 'b').length).toBe(3)
  })

  it('reports schema violations per document with a path', () => {
    const { objects, errors } = parseApplyYaml(`
apiVersion: klite/v1
kind: Workload
metadata: { name: bad }
spec:
  replicas: -1
  template:
    labels: { app: bad }
    containers:
      - name: x
        image: img
`)
    expect(objects).toEqual([])
    expect(errors.length).toBe(1)
    expect(errors[0].action).toBe('error')
    expect(errors[0].error).toContain('replicas')
  })

  it('rejects applying Instances — they are server-materialized', () => {
    const c = new Cluster(seedObjects())
    const results = c.applyObjects([
      {
        apiVersion: 'klite/v1',
        kind: 'Instance',
        metadata: { name: 'rogue-1' },
        spec: { workload: 'rogue', container: { name: 'x', image: 'img' } },
        status: { phase: 'Pending', restarts: 0 },
      },
    ])
    expect(results[0].action).toBe('error')
  })
})

describe('policyCheck parity', () => {
  it('returns the identical verdict the traffic path produces', () => {
    const c = new Cluster(seedObjects(), DENSE)
    settle(c, 4000)
    c.applyObjects(
      parseApplyYaml(`
apiVersion: klite/v1
kind: NetworkPolicy
metadata: { name: lockdown-a }
spec:
  action: DENY
  rules:
    - { from: a, to: "*", except: [b] }
`).objects,
    )
    const check = c.policyCheck('a', 'c')
    const events: TrafficEvent[] = []
    c.subscribeTraffic((e) => events.push(e))
    settle(c, 90000)
    const observed = events.find((e) => e.fromService === 'a' && e.toService === 'c')
    expect(check.allowed).toBe(false)
    expect(observed?.matchedRule).toEqual(check.matchedRule)
    // and the except-list carve-out really flows
    const aToB = events.filter((e) => e.fromService === 'a' && e.toService === 'b')
    expect(aToB.length).toBeGreaterThan(0)
    expect(aToB.every((e) => e.verdict === 'allowed')).toBe(true)
  })
})

describe('workload updates', () => {
  it('an image change alone reports updated, not unchanged', () => {
    const c = new Cluster(seedObjects())
    settle(c, 2000)
    const wl = c.get('Workload', 'b') as Workload
    wl.spec.template.containers[0].image = 'traefik/whoami:v1.11'
    expect(c.applyObjects([wl])[0].action).toBe('updated')
    expect(c.applyObjects([wl])[0].action).toBe('unchanged')
  })
})
