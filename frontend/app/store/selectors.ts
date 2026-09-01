// These selectors derive views from a Snapshot without touching it. Endpoints
// are computed rather than stored, exactly how EDS derives them from instance
// records.

import {
  endpointStateOf,
  type Instance,
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
