// These selectors derive views from a Snapshot without touching it. Endpoints
// are computed rather than stored, exactly how EDS derives them from instance
// records.

import {
  endpointStateOf,
  type Instance,
  type NodeObj,
  type Service,
  selectorMatches,
  type Workload,
} from '@/api/types'
import type { Snapshot } from './store'

export function instancesByNode(s: Snapshot): Map<string, Instance[]> {
  const out = new Map<string, Instance[]>()
  for (const inst of Object.values(s.instances)) {
    if (!inst.spec.node) continue
    const list = out.get(inst.spec.node) ?? []
    list.push(inst)
    out.set(inst.spec.node, list)
  }
  for (const list of out.values()) list.sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
  return out
}

export function pendingInstances(s: Snapshot): Instance[] {
  return Object.values(s.instances)
    .filter((i) => !i.spec.node)
    .sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
}

export { selectorMatches } from '@/api/types'

export interface Endpoints {
  ready: Instance[]
  draining: Instance[]
}

// Snapshots are immutable, so per-snapshot memos are safe. Every node card
// derives endpoints for every service on every render, and without the memo
// that work is quadratic in cluster size.
const endpointsCache = new WeakMap<Snapshot, Map<string, Endpoints>>()

export function endpointsOf(s: Snapshot, svc: Service): Endpoints {
  let perService = endpointsCache.get(s)
  if (!perService) {
    perService = new Map()
    endpointsCache.set(s, perService)
  }
  const hit = perService.get(svc.metadata.name)
  if (hit) return hit
  const out: Endpoints = { ready: [], draining: [] }
  for (const inst of Object.values(s.instances)) {
    if (!selectorMatches(svc.spec.selector, inst.metadata.labels)) continue
    const state = endpointStateOf(inst)
    if (state === 'READY') out.ready.push(inst)
    else if (state === 'DRAINING') out.draining.push(inst)
  }
  perService.set(svc.metadata.name, out)
  return out
}

export function serviceOfWorkload(s: Snapshot, wl: Workload): Service | undefined {
  return Object.values(s.services).find((svc) =>
    selectorMatches(svc.spec.selector, wl.spec.template.labels),
  )
}

export interface WorkloadRow {
  workload: Workload
  ready: number
  total: number
  service?: Service
}

export function workloadRows(s: Snapshot): WorkloadRow[] {
  return Object.values(s.workloads)
    .sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
    .map((workload) => {
      const mine = Object.values(s.instances).filter((i) => i.spec.workload === workload.metadata.name)
      return {
        workload,
        ready: mine.filter((i) => i.status.phase === 'Ready').length,
        total: mine.length,
        service: serviceOfWorkload(s, workload),
      }
    })
}

export function sortedNodes(s: Snapshot) {
  return Object.values(s.nodes).sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
}

export function sortedServices(s: Snapshot) {
  return Object.values(s.services).sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
}

export function sortedPolicies(s: Snapshot) {
  return Object.values(s.policies).sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
}

export function sortedInstances(s: Snapshot) {
  return Object.values(s.instances).sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
}

export function serviceOfInstance(s: Snapshot, inst: Instance): Service | undefined {
  return Object.values(s.services).find((svc) =>
    selectorMatches(svc.spec.selector, inst.metadata.labels),
  )
}

// The identity map Envoy gets over xDS: source IP → instance → service.
// This is how the RBAC filter knows who is calling.
export interface IdentityRow {
  ip: string
  instance: string
  service: string
}

export function identityRows(s: Snapshot): IdentityRow[] {
  return sortedInstances(s)
    .filter((i) => i.status.instanceIp)
    .map((i) => ({
      ip: i.status.instanceIp as string,
      instance: i.metadata.name,
      service: serviceOfInstance(s, i)?.metadata.name ?? i.spec.workload,
    }))
}

// vipFor answers the per-node VIP a caller resolves, reading VIPAllocation
// objects the way kdns compiles its table (ADR 0022). The memo lives per
// immutable snapshot.
const vipCache = new WeakMap<Snapshot, Map<string, string>>()

export function vipFor(s: Snapshot, service: string, node: string): string | undefined {
  let table = vipCache.get(s)
  if (!table) {
    table = new Map()
    for (const va of Object.values(s.vipAllocations)) {
      table.set(`${va.spec.service}|${va.spec.node}`, va.spec.vip)
    }
    vipCache.set(s, table)
  }
  return table.get(`${service}|${node}`)
}

// infraIpOf derives the infra pod's donor IP exactly as the server assigns
// it: 10.44.0.<10 + nodeIndex>, indexes starting at 1
// (internal/server/agent.go). A node with no index hasn't registered, so
// there's nothing to show.
export function infraIpOf(node: NodeObj): string | undefined {
  if (node.status?.infra?.ip) return node.status.infra.ip
  const idx = node.status?.nodeIndex
  return idx ? `10.44.0.${10 + idx}` : undefined
}

// dialTargetOf answers what `fromNode`'s Envoy would actually dial for one
// endpoint: the raw instance ip:targetPort when it's local, or the owning
// node's advertised machine address and mTLS ingress port when it's remote
// (M9).
export function dialTargetOf(
  s: Snapshot,
  svc: Service,
  inst: Instance,
  fromNode: string,
): { local: boolean; address: string } {
  const local = inst.spec.node === fromNode
  if (local) {
    return { local, address: `${inst.status.instanceIp ?? '?'}:${svc.spec.targetPort}` }
  }
  const machine = (inst.spec.node && s.nodes[inst.spec.node]?.status?.advertiseAddress) ?? '?'
  const port = inst.status.ingressPort
  return { local, address: port ? `${machine}:${port}` : `${machine}` }
}

// ingressRowsOf lists what one node publishes: each local endpoint's mTLS
// ingress port and the raw instance address it forwards to, which is the
// DNAT table remote proxies depend on.
export interface IngressRow {
  port: number
  instance: string
  forward: string
}

export function ingressRowsOf(s: Snapshot, node: string): IngressRow[] {
  const rows: IngressRow[] = []
  for (const svc of sortedServices(s)) {
    const eps = endpointsOf(s, svc)
    for (const inst of [...eps.ready, ...eps.draining]) {
      if (inst.spec.node !== node || !inst.status.ingressPort) continue
      rows.push({
        port: inst.status.ingressPort,
        instance: inst.metadata.name,
        forward: `${inst.status.instanceIp ?? '?'}:${svc.spec.targetPort}`,
      })
    }
  }
  return rows.sort((a, b) => a.port - b.port)
}

// The compiled RBAC view every node's Envoy enforces: DENY rules first, then
// ALLOW rules, then the default line.
export interface RbacView {
  deny: { policy: string; from: string; to: string; except?: string[] }[]
  allow: { policy: string; from: string; to: string; except?: string[] }[]
}

const rbacCache = new WeakMap<Snapshot, RbacView>()

export function rbacView(s: Snapshot): RbacView {
  const cached = rbacCache.get(s)
  if (cached) return cached
  const view: RbacView = { deny: [], allow: [] }
  for (const p of sortedPolicies(s)) {
    const bucket = p.spec.action === 'DENY' ? view.deny : view.allow
    for (const r of p.spec.rules) {
      bucket.push({ policy: p.metadata.name, from: r.from, to: r.to, except: r.except })
    }
  }
  rbacCache.set(s, view)
  return view
}
