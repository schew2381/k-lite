// buildTrace turns one TrafficEvent into the story of its request, with the
// dot moving exactly when packets do: the DNS query rides out to kdns, the
// answer rides back, then the instance itself dials the VIP — two separate
// exchanges, because that's how the data path works (ADR 0006/0008: kdns
// answers a VIP that Envoy has bound, and nothing intercepts or rewrites).
// The trace panel prints one short line per step. Wording is
// platform-generic: resolvers, not Docker.

import type { TrafficEvent } from '@/api/types'
import { vipFor } from '@/store/selectors'
import type { Snapshot } from '@/store/store'
import type { Trace, TraceStep } from '@/store/traceStore'

export type { Trace, TraceStep } from '@/store/traceStore'

export function buildTrace(e: TrafficEvent, s: Snapshot): Trace {
  const svc = s.services[e.toService]
  const vip = vipFor(s, e.toService, e.viaNode) ?? '10.44.64.?'
  const port = svc?.spec.port ?? 8080
  const callerIp = s.instances[e.fromInstance]?.status.instanceIp ?? '10.44.128.?'
  const targetNode = e.toInstance ? (s.instances[e.toInstance]?.spec.node ?? '?') : undefined

  const steps: TraceStep[] = [
    {
      at: 'caller',
      short: `resolve ${e.toService}`,
      detail: `${e.fromInstance} asks for ${e.toService}. Its resolver expands it to ${e.toService}.svc.klite.`,
      tone: 'info',
    },
    {
      at: 'kdns',
      motion: 'travel',
      short: `→ ${e.viaNode} kdns`,
      detail: `The query goes to its one upstream: ${e.viaNode}'s kdns.`,
      tone: 'info',
    },
    {
      at: 'caller',
      motion: 'travel',
      short: `${vip} · TTL 5s`,
      detail: `The answer rides back: ${vip} (TTL 5s), ${e.viaNode}'s VIP for ${e.toService}. Nothing has dialed yet.`,
      tone: 'info',
    },
    {
      at: 'lds',
      motion: 'travel',
      short: `dial ${vip}:${port}`,
      detail: `${e.fromInstance} itself dials ${vip}:${port} — the VIP is Envoy's listener address, so nothing intercepts or rewrites the connection.`,
      tone: 'info',
    },
  ]

  if (e.verdict === 'denied' && e.reason === 'policy') {
    steps.push({
      at: 'rbac',
      short: `RBAC ✕ ${e.matchedRule?.policy ?? 'denied'}`,
      detail: `RBAC (${callerIp} = ${e.fromService}): ${e.matchedRule?.policy ?? 'a policy'} denies ${e.fromService} → ${e.toService}. Connection reset.`,
      tone: 'deny',
    })
    return { event: e, steps }
  }

  steps.push({
    at: 'rbac',
    short: e.matchedRule ? `RBAC ✓ ${e.matchedRule.policy}` : 'RBAC ✓ allow',
    detail: e.matchedRule
      ? `RBAC (${callerIp} = ${e.fromService}): ${e.matchedRule.policy} allows ${e.fromService} → ${e.toService}.`
      : `RBAC (${callerIp} = ${e.fromService}): no DENY matches and no ALLOW targets ${e.toService}, so the call passes.`,
    tone: 'allow',
  })

  if (e.verdict === 'denied') {
    steps.push({
      at: 'eds',
      short: 'no endpoints',
      detail: `EDS has no READY endpoints for ${e.toService} right now. The dial fails fast.`,
      tone: 'deny',
    })
    return { event: e, steps }
  }

  steps.push({
    at: 'eds',
    short: `pick ${e.toInstance}`,
    detail: `EDS round-robin over READY endpoints picks ${e.toInstance}. DRAINING gets nothing new.`,
    tone: 'info',
  })
  steps.push({
    at: 'target',
    motion: 'travel',
    short: `→ ${e.toInstance}`,
    detail: `Envoy relays the bytes to ${e.toInstance}${targetNode ? ` on ${targetNode}` : ''} (${e.latencyMs}ms).`,
    tone: 'allow',
  })
  return { event: e, steps }
}
