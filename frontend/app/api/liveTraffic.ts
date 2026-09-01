// The live feed (GET /api/traffic, ADR 0041) carries Envoy counter deltas:
// caller's node, target service, and the exact dial target. This module
// turns one delta into TrafficEvents by looking the dial target up in the
// snapshot — an instance IP means a local call, while an ingress port
// names the instance behind a machine address. The caller's instance is
// unknown by construction, since one Envoy fronts every instance on its
// node. fromInstance stays empty, and the UI attributes the call to the node.

import type { Snapshot } from '@/store/store'
import type { TrafficEvent } from './types'

export interface TrafficDelta {
  unixMs: number
  node: string
  service: string
  address: string
  port: number
  count: number
}

// One delta row can cover several calls. More dots than this per row per
// second is noise, not information.
const MAX_EVENTS_PER_DELTA = 4

let seq = 0

export function enrichTrafficDelta(raw: TrafficDelta, s: Snapshot): TrafficEvent[] {
  if (!s.services[raw.service]) return []

  let toInstance: string | undefined
  const byIp = Object.values(s.instances).find((i) => i.status.instanceIp === raw.address)
  if (byIp) {
    toInstance = byIp.metadata.name
  } else {
    const alloc = Object.values(s.ingressAllocations).find(
      (a) => a.spec.port === raw.port && a.spec.service === raw.service,
    )
    toInstance = alloc?.spec.instance
  }

  const events: TrafficEvent[] = []
  for (let i = 0; i < Math.min(raw.count, MAX_EVENTS_PER_DELTA); i++) {
    events.push({
      id: `live-${raw.unixMs}-${seq++}`,
      ts: raw.unixMs,
      fromInstance: '',
      fromService: '',
      toService: raw.service,
      viaNode: raw.node,
      verdict: 'allowed',
      toInstance,
    })
  }
  return events
}
