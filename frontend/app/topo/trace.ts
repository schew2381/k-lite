// buildTrace turns one TrafficEvent into the story of its request, with the
// dot moving exactly when packets do: the DNS query rides out to kdns, the
// answer rides back, then the instance itself dials the VIP — two separate
// exchanges, because that's how the data path works (ADR 0006/0008: kdns
// answers a VIP that Envoy has bound, and nothing intercepts or rewrites).
// The trace panel prints one short line per step. Wording is
// platform-generic: resolvers, not Docker.

import type { TrafficEvent } from '@/api/types'
import { ingressPortOf, vipFor } from '@/store/selectors'
import type { Snapshot } from '@/store/store'
import type { Trace, TraceStep } from '@/store/traceStore'

export type { Trace, TraceStep } from '@/store/traceStore'

// anchorIdFor names the layout anchor a step stands on. The dot layer asks
// it where flights start and land.
export function anchorIdFor(at: TraceStep['at'], trace: Trace): string {
  const e = trace.event
  if (at === 'caller') return `instance:${e.fromInstance}`
  if (at === 'target') return `instance:${e.toInstance}`
  if (at === 'targetInfra') return `ingress:${trace.targetNode}`
  return `${at}:${e.viaNode}` // kdns | lds | rbac | eds sub-box on the caller's node
}

export function buildTrace(e: TrafficEvent, s: Snapshot): Trace {
  const svc = s.services[e.toService]
  const vip = vipFor(s, e.toService, e.viaNode) ?? '10.44.64.?'
  const port = svc?.spec.port ?? 8080
  const callerIp = s.instances[e.fromInstance]?.status.instanceIp ?? '10.44.128.?'
  const rbacWho = e.fromService ? `${callerIp} = ${e.fromService}` : `a caller on ${e.viaNode}`
  const caller = e.fromService || 'the caller'
  const took = e.latencyMs !== undefined ? ` (${e.latencyMs}ms)` : ''
  const targetNode = e.toInstance ? (s.instances[e.toInstance]?.spec.node ?? '?') : undefined

  // A live-feed event knows the caller's node but not its instance (one
  // Envoy fronts them all), so its story starts at kdns instead of a chip.
  const steps: TraceStep[] = e.fromInstance
    ? [
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
    : [
        {
          at: 'kdns',
          short: `resolve ${e.toService}`,
          detail: `An instance on ${e.viaNode} asks kdns for ${e.toService} and gets ${vip} back.`,
          tone: 'info',
        },
        {
          at: 'lds',
          motion: 'travel',
          short: `dial ${vip}:${port}`,
          detail: `The caller dials ${vip}:${port}, Envoy's listener address for ${e.toService}.`,
          tone: 'info',
        },
      ]

  const remote = targetNode !== undefined && targetNode !== e.viaNode

  if (e.verdict === 'denied' && e.reason === 'policy') {
    steps.push({
      at: 'rbac',
      short: `RBAC ✕ ${e.matchedRule?.policy ?? 'denied'}`,
      detail: `RBAC (${rbacWho}): ${e.matchedRule?.policy ?? 'a policy'} denies ${caller} → ${e.toService}. Connection reset.`,
      tone: 'deny',
    })
    return { event: e, steps, targetNode }
  }

  steps.push({
    at: 'rbac',
    // the cross-node story is long already, and this line reads in a moment
    ...(remote && { pace: 'short' as const }),
    short: e.matchedRule ? `RBAC ✓ ${e.matchedRule.policy}` : 'RBAC ✓ allow',
    detail: e.matchedRule
      ? `RBAC (${rbacWho}): ${e.matchedRule.policy} allows ${caller} → ${e.toService}.`
      : `RBAC (${rbacWho}): no DENY matches and no ALLOW targets ${e.toService}, so the call passes.`,
    tone: 'allow',
  })

  if (e.verdict === 'denied') {
    steps.push({
      at: 'eds',
      short: 'no endpoints',
      detail: `EDS has no READY endpoints for ${e.toService} right now. The dial fails fast.`,
      tone: 'deny',
    })
    return { event: e, steps, targetNode }
  }

  if (!remote) {
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
      detail: `Envoy relays the bytes to ${e.toInstance}${targetNode ? ` on ${targetNode}` : ''}${took}.`,
      tone: 'allow',
    })
    return { event: e, steps, targetNode }
  }

  // The pick is on another machine, so the EDS entry is that node's
  // advertised address instead of an instance IP (M9's ingress design).
  const machine = (targetNode && s.nodes[targetNode]?.status?.advertiseAddress) ?? '198.51.100.?'
  const ingress = e.toInstance ? ingressPortOf(s, e.toService, e.toInstance) : undefined
  const via = ingress ? `${machine}:${ingress}` : `${machine} ingress`
  steps.push({
    at: 'eds',
    short: `pick ${e.toInstance} · remote`,
    detail: `EDS picks ${e.toInstance} on ${targetNode}. It lives on another machine, so the entry is ${via} instead of an instance IP.`,
    tone: 'info',
  })
  steps.push({
    at: 'targetInfra',
    motion: 'travel',
    pace: 'long',
    short: `mTLS → ${via}`,
    detail: `${e.viaNode}'s Envoy dials ${via} across the open internet, inside proxy-to-proxy mTLS.`,
    tone: 'info',
  })
  steps.push({
    at: 'targetInfra',
    pace: 'short',
    short: ingress ? `DNAT :${ingress} → envoy` : 'DNAT → envoy ingress',
    detail: `The machine's published port lands the connection inside ${targetNode}'s infra pod (the DNAT hop), where Envoy terminates the mTLS.`,
    tone: 'info',
  })
  steps.push({
    at: 'target',
    motion: 'travel',
    short: `→ ${e.toInstance}`,
    detail: `${targetNode}'s Envoy hands the bytes to ${e.toInstance}${took}, since the owning node dials its instances raw.`,
    tone: 'allow',
  })
  return { event: e, steps, targetNode }
}
