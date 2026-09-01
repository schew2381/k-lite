// createMockClient wraps the in-browser control plane (sim/cluster.ts) in the
// KliteClient interface. The store can't tell it apart from the real thing,
// because the watch replay, event shapes, and verdicts are identical.

import { Cluster } from '@/sim/cluster'
import { seedObjects } from '@/sim/seed'
import type { KliteClient } from './client'
import { parseApplyYaml } from './schemas'
import type { Kind, TopologySnapshot } from './types'

const TICK_MS = 100
const MAX_STEP_MS = 1000 // background tabs clamp timers, so one firing must not warp time

export function createMockClient(): KliteClient {
  const cluster = new Cluster(seedObjects())
  let speed = 1
  let last = performance.now()

  const timer = setInterval(() => {
    const now = performance.now()
    const dt = Math.min(now - last, MAX_STEP_MS)
    last = now
    if (speed > 0) cluster.advance(dt * speed)
  }, TICK_MS)

  return {
    mode: 'mock',

    async apply(yamlText) {
      const { objects, errors } = parseApplyYaml(yamlText)
      return [...errors, ...cluster.applyObjects(objects)]
    },
    async list(kind: Kind) {
      return cluster.list(kind)
    },
    async get(kind, name) {
      return cluster.get(kind, name)
    },
    async remove(kind, name) {
      cluster.remove(kind, name)
    },
    async scale(workload, replicas) {
      const wl = cluster.get('Workload', workload)
      if (wl?.kind !== 'Workload') return
      wl.spec.replicas = replicas
      cluster.applyObjects([wl])
    },
    async drainNode(node) {
      cluster.drainNode(node)
    },
    async cordon(node, on) {
      cluster.cordon(node, on)
    },

    watch(onEvent) {
      return cluster.subscribe(onEvent)
    },
    watchTraffic(onEvent) {
      return cluster.subscribeTraffic(onEvent)
    },
    streamLogs(instance, onLine) {
      return cluster.logs.subscribe(instance, onLine)
    },

    async policyCheck(from, to) {
      return cluster.policyCheck(from, to)
    },
    async topology(): Promise<TopologySnapshot> {
      const kinds: Kind[] = ['Node', 'Workload', 'Service', 'NetworkPolicy', 'Instance']
      return { rev: cluster.currentRev(), objects: kinds.flatMap((k) => cluster.list(k)) }
    },
    async killInstance(name) {
      cluster.killInstance(name)
    },
    async health() {
      return { ok: true }
    },
    dispose() {
      clearInterval(timer)
    },

    chaos: {
      killNodeAgent: (n) => cluster.killNodeAgent(n),
      reviveNodeAgent: (n) => cluster.reviveNodeAgent(n),
      setSpeed: (x) => {
        speed = Math.max(0, x)
      },
      speed: () => speed,
    },
  }
}
