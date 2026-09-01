// Traffic events are ephemeral and frequent, so they live outside the snapshot
// in a small ring. The rail and the reduced-motion fallback read it, and
// nothing re-renders the topology per call.

import { useSyncExternalStore } from 'react'
import type { TrafficEvent } from '@/api/types'

const CAP = 200

class TrafficLog {
  private ring: TrafficEvent[] = []
  private view: TrafficEvent[] = []
  private listeners = new Set<() => void>()
  private notifyQueued = false

  push = (e: TrafficEvent) => {
    this.ring.push(e)
    if (this.ring.length > CAP) this.ring.splice(0, this.ring.length - CAP)
    if (this.notifyQueued) return
    this.notifyQueued = true
    queueMicrotask(() => {
      this.notifyQueued = false
      this.view = [...this.ring]
      for (const l of this.listeners) l()
    })
  }

  // a client swap starts a fresh story, so the old source's calls go
  clear = () => {
    this.ring = []
    this.view = []
    for (const l of this.listeners) l()
  }

  subscribe = (cb: () => void) => {
    this.listeners.add(cb)
    return () => this.listeners.delete(cb)
  }

  getSnapshot = (): TrafficEvent[] => this.view
}

export const trafficLog = new TrafficLog()

export function useTrafficLog(): TrafficEvent[] {
  return useSyncExternalStore(trafficLog.subscribe, trafficLog.getSnapshot)
}
