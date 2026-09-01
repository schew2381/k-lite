// ClusterStore replays the watch stream into an immutable snapshot, and that
// snapshot is the only client-side state. Every view derives from it, so the
// UI shows what the control plane says rather than what a button handler
// hoped.

import { useSyncExternalStore } from 'react'
import type {
  IngressAllocation,
  Instance,
  Kind,
  KliteObject,
  NetworkPolicy,
  NodeObj,
  Service,
  VIPAllocation,
  WatchEvent,
  Workload,
} from '@/api/types'

export interface Snapshot {
  rev: number
  synced: boolean
  workloads: Record<string, Workload>
  services: Record<string, Service>
  nodes: Record<string, NodeObj>
  instances: Record<string, Instance>
  policies: Record<string, NetworkPolicy>
  vipAllocations: Record<string, VIPAllocation>
  ingressAllocations: Record<string, IngressAllocation>
}

const EMPTY: Snapshot = {
  rev: 0,
  synced: false,
  workloads: {},
  services: {},
  nodes: {},
  instances: {},
  policies: {},
  vipAllocations: {},
  ingressAllocations: {},
}

type ObjectMapKey = keyof Pick<
  Snapshot,
  'workloads' | 'services' | 'nodes' | 'instances' | 'policies' | 'vipAllocations' | 'ingressAllocations'
>

const KEY: Record<Kind, ObjectMapKey> = {
  Workload: 'workloads',
  Service: 'services',
  Node: 'nodes',
  NetworkPolicy: 'policies',
  Instance: 'instances',
  VIPAllocation: 'vipAllocations',
  IngressAllocation: 'ingressAllocations',
}

export class ClusterStore {
  private snapshot: Snapshot = EMPTY
  private listeners = new Set<() => void>()
  private notifyQueued = false

  applyEvent = (e: WatchEvent) => {
    if (e.type === 'RESET') {
      this.snapshot = { ...EMPTY }
      this.queueNotify()
      return
    }
    if (e.type === 'SYNC') {
      this.snapshot = { ...this.snapshot, rev: e.rev, synced: true }
      this.queueNotify()
      return
    }
    if (!e.kind || !e.object) return
    const key = KEY[e.kind]
    const map: Record<string, KliteObject> = { ...this.snapshot[key] }
    if (e.type === 'DELETED') delete map[e.object.metadata.name]
    else map[e.object.metadata.name] = e.object
    this.snapshot = { ...this.snapshot, rev: e.rev, [key]: map }
    this.queueNotify()
  }

  // Bursts (the initial replay, a drain cascade) collapse into one render.
  private queueNotify() {
    if (this.notifyQueued) return
    this.notifyQueued = true
    queueMicrotask(() => {
      this.notifyQueued = false
      for (const l of this.listeners) l()
    })
  }

  subscribe = (cb: () => void) => {
    this.listeners.add(cb)
    return () => this.listeners.delete(cb)
  }

  getSnapshot = (): Snapshot => this.snapshot
}

export const clusterStore = new ClusterStore()

export function useSnapshot(): Snapshot {
  return useSyncExternalStore(clusterStore.subscribe, clusterStore.getSnapshot)
}
