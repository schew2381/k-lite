// useFlow picks between ADR 0027's two traffic flows, and the flow simply
// follows the data source: mock plays the traced walkthrough and a live
// cluster plays live. Tests preview live pacing on mock data through the
// window.__kliteFlow hook, the same pattern as the sim-speed hook.

import { useSyncExternalStore } from 'react'
import { useClient } from '@/lib/client-context'

export type FlowMode = 'traced' | 'live'

let override: FlowMode | null = null
const listeners = new Set<() => void>()

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

declare global {
  interface Window {
    __kliteFlow?: (flow: FlowMode | null) => void
  }
}

if (typeof window !== 'undefined') {
  window.__kliteFlow = (flow) => {
    override = flow
    for (const l of listeners) l()
  }
}

export function useFlow(): FlowMode {
  const client = useClient()
  const forced = useSyncExternalStore(subscribe, () => override)
  if (forced) return forced
  return client.mode === 'http' ? 'live' : 'traced'
}
