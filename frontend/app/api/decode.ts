// The facade speaks two JSON dialects. Lists and apply results pass through
// internal/object's user-facing codec (metadata key, "ALLOW"/"DENY", the kind
// on the envelope), while watch events are raw protojson (a meta key, the
// Object oneof wrapper, and enum value names like "INSTANCE_PHASE_READY").
// decodeObject normalizes both into the app's canonical KliteObject, so
// nothing outside this file knows the wire had dialects at all.

import type { Kind, KliteObject, ObjectMeta } from './types'

// protojson oneof field name → kind, e.g. {"workload": {...}} → Workload
const ONEOF_KIND: Record<string, Kind> = {
  workload: 'Workload',
  service: 'Service',
  node: 'Node',
  networkPolicy: 'NetworkPolicy',
  instance: 'Instance',
  vipAllocation: 'VIPAllocation',
}

// list path segment → kind, matching internal/object's plural table
export const KIND_PATH: Record<Kind, string> = {
  Workload: 'workloads',
  Service: 'services',
  Node: 'nodes',
  NetworkPolicy: 'networkpolicies',
  Instance: 'instances',
  VIPAllocation: 'vipallocations',
}

// Turns "INSTANCE_PHASE_NOT_READY" into "NotReady", the same rule the
// facade's topology composer applies. Values already in display form pass
// through.
function displayEnum(value: unknown): unknown {
  if (typeof value !== 'string' || !/^[A-Z0-9_]+$/.test(value)) return value
  const words = value.split('_')
  // drop the message-name prefix: everything up to and including PHASE/ACTION
  const marker = words.findIndex((w) => w === 'PHASE' || w === 'ACTION')
  const rest = marker >= 0 ? words.slice(marker + 1) : words
  if (rest.length === 0 || rest[0] === 'UNSPECIFIED') return 'Unknown'
  return rest.map((w) => w[0] + w.slice(1).toLowerCase()).join('')
}

function toMeta(raw: Record<string, unknown>): ObjectMeta {
  const rv = raw.resourceVersion
  return {
    name: String(raw.name ?? ''),
    labels: raw.labels as Record<string, string> | undefined,
    uid: raw.uid as string | undefined,
    // protojson renders int64 as a string, and the codec may keep it numeric
    resourceVersion: rv === undefined ? undefined : Number(rv),
    createdUnix: raw.createdUnix === undefined ? undefined : Number(raw.createdUnix),
  }
}

interface RawObject {
  kind?: string
  metadata?: Record<string, unknown>
  meta?: Record<string, unknown>
  [key: string]: unknown
}

// decodeObject accepts either dialect and returns the canonical object, or
// null when the payload carries no recognizable kind.
export function decodeObject(raw: unknown): KliteObject | null {
  if (typeof raw !== 'object' || raw === null) return null
  let body = raw as RawObject
  let kind = body.kind as Kind | undefined

  // protojson Object wrapper: exactly one oneof field names the kind
  if (!kind) {
    for (const [field, k] of Object.entries(ONEOF_KIND)) {
      if (body[field] && typeof body[field] === 'object') {
        kind = k
        body = body[field] as RawObject
        break
      }
    }
  }
  if (!kind || !(kind in KIND_PATH)) return null

  const metaRaw = (body.metadata ?? body.meta ?? {}) as Record<string, unknown>
  const spec = (body.spec ?? {}) as Record<string, unknown>
  const status = body.status as Record<string, unknown> | undefined

  const obj = {
    apiVersion: 'klite/v1',
    kind,
    metadata: toMeta(metaRaw),
    spec,
    ...(status !== undefined && { status }),
  } as unknown as KliteObject

  // enum-name fields the protojson dialect carries, harmless on the other
  if (status?.phase !== undefined) status.phase = displayEnum(status.phase)
  if (kind === 'NetworkPolicy' && typeof spec.action === 'string') {
    const action = spec.action.replace(/^POLICY_ACTION_/, '')
    spec.action = action === 'UNSPECIFIED' ? 'UNKNOWN' : action
  }
  return obj
}
