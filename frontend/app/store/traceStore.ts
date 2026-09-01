// The active trace, shared between the dot layer (which drives it) and the
// trace panel (which prints it). It lives outside React state for the same
// reason the traffic ring does: the dot layer updates it from rAF callbacks.

import { useSyncExternalStore } from 'react'
import type { TrafficEvent } from '@/api/types'

// The step and trace shapes live here, not in topo/, so the store never
// imports from the component layer. topo/trace.ts builds them.
export interface TraceStep {
  // where the dot is when this step ends: an instance, an infra sub-box on
  // the caller's node, or the target node's whole infra pod
  at: 'caller' | 'kdns' | 'lds' | 'rbac' | 'eds' | 'targetInfra' | 'target'
  // 'travel': the step is told while the dot moves to `at`, not parked there
  motion?: 'travel'
  // short label riding on the dot, e.g. "kdns: 10.44.64.7"
  short: string
  // one glanceable line for the trace panel
  detail: string
  tone: 'info' | 'allow' | 'deny'
}

export interface Trace {
  event: TrafficEvent
  steps: TraceStep[]
  targetNode?: string // where the picked endpoint lives, anchoring the remote hops
}

export interface ActiveTrace {
  trace: Trace
  stepIndex: number // index of the step being told right now
  done: boolean
}

class TraceStore {
  private state: ActiveTrace | null = null
  private listeners = new Set<() => void>()

  set = (state: ActiveTrace | null) => {
    this.state = state
    for (const l of this.listeners) l()
  }

  step = (stepIndex: number) => {
    if (!this.state) return
    this.state = { ...this.state, stepIndex }
    for (const l of this.listeners) l()
  }

  finish = () => {
    if (!this.state) return
    this.state = { ...this.state, done: true, stepIndex: this.state.trace.steps.length - 1 }
    for (const l of this.listeners) l()
  }

  busy = (): boolean => this.state !== null && !this.state.done

  subscribe = (cb: () => void) => {
    this.listeners.add(cb)
    return () => this.listeners.delete(cb)
  }

  getSnapshot = (): ActiveTrace | null => this.state
}

export const traceStore = new TraceStore()

export function useActiveTrace(): ActiveTrace | null {
  return useSyncExternalStore(traceStore.subscribe, traceStore.getSnapshot)
}
