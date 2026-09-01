// createMockClient wraps the in-browser control plane (sim/cluster.ts) in the
// KliteClient interface. The store can't tell it apart from the real thing,
// because the watch replay, event shapes, and verdicts are identical.

import { Cluster } from '@/sim/cluster'
import { toPolicyVerdict } from '@/sim/policy'
import { seedObjects } from '@/sim/seed'
import type { KliteClient } from './client'
import { parseApplyYaml } from './schemas'
import { type Kind, selectorMatches, type Topology } from './types'

const TICK_MS = 100
const MAX_STEP_MS = 1000 // background tabs clamp timers, so one firing must not warp time

// composeTopology mirrors the facade's ComposeTopology: the same graph from
// the same five lists, so a page written against one answers to the other.
function composeTopology(cluster: Cluster): Topology {
  const workloads = cluster.list('Workload').filter((o) => o.kind === 'Workload')
  const instances = cluster.list('Instance').filter((o) => o.kind === 'Instance')
  const nodes = cluster.list('Node').filter((o) => o.kind === 'Node')
  const services = cluster.list('Service').filter((o) => o.kind === 'Service')
  const policies = cluster.list('NetworkPolicy').filter((o) => o.kind === 'NetworkPolicy')

  const templateLabels = new Map(workloads.map((w) => [w.metadata.name, w.spec.template.labels]))
  const toTopo = (i: (typeof instances)[number]) => ({
    name: i.metadata.name,
    workload: i.spec.workload,
    phase: i.status.phase,
    restarts: i.status.restarts,
    ip: i.status.instanceIp ?? '',
  })
  // Running counts as routable alongside Ready, matching the facade's note
  const routable = new Set(
    instances
      .filter((i) => i.status.phase === 'Running' || i.status.phase === 'Ready')
      .map((i) => i.metadata.name),
  )

  return {
    nodes: nodes.map((n) => ({
      name: n.metadata.name,
      phase: n.status?.phase ?? 'Unknown',
      unschedulable: n.status?.unschedulable ?? false,
      instances: instances.filter((i) => i.spec.node === n.metadata.name).map(toTopo),
    })),
    services: services.map((s) => ({
      name: s.metadata.name,
      port: s.spec.port,
      targetPort: s.spec.targetPort,
      selector: s.spec.selector,
      endpoints: instances
        .filter(
          (i) =>
            routable.has(i.metadata.name) &&
            selectorMatches(s.spec.selector, templateLabels.get(i.spec.workload)),
        )
        .map((i) => i.metadata.name)
        .sort(),
    })),
    policies: policies.map((p) => ({
      name: p.metadata.name,
      action: p.spec.action,
      rules: p.spec.rules,
    })),
    workloads: workloads.map((w) => ({
      name: w.metadata.name,
      ready: w.status?.readyInstances ?? 0,
      total: w.spec.replicas,
    })),
    unscheduled: instances.filter((i) => !i.spec.node).map(toTopo),
  }
}

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
    can: { cordon: true, uncordon: true, drain: true },

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
      return toPolicyVerdict(cluster.policyCheck(from, to), from, to)
    },
    async topology(): Promise<Topology> {
      return composeTopology(cluster)
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
