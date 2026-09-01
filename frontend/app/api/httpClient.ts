// HttpClient speaks the REST/SSE facade from ADR 0015. klited doesn't serve
// it yet, so this skeleton doubles as the wire contract klited must meet.
//
// ── Facade contract ─────────────────────────────────────────────────────────
// POST /v1/apply                     body: raw multi-doc YAML
//                                    → 200 [{kind, name, action, error?}]
// GET  /v1/{kind}                    kind path segment is lowercase plural:
//                                    workloads, services, nodes,
//                                    networkpolicies, instances
// GET|DELETE /v1/{kind}/{name}       DELETE of a node runs the drain
//                                    choreography before removal (ADR 0010)
// GET  /v1/watch                     SSE. Each event:
//                                      id: <rev>
//                                      event: watch
//                                      data: {"type":"ADDED|MODIFIED|DELETED|SYNC|RESET",
//                                             "rev":n, "kind":..., "object":{...}}
//                                    On connect: ADDED for every object, then
//                                    {"type":"SYNC"}. Resume with Last-Event-ID.
//                                    If compacted past it, the server sends
//                                    {"type":"RESET"} and a fresh replay.
//                                    Comment-line ping every 15s.
// GET  /v1/traffic                   SSE of TrafficEvent JSON, fed from Envoy
//                                    access logs. NOT in ADR 0015's frozen
//                                    list — added by ADR 0024. A 404 here
//                                    means "not implemented yet" and the UI
//                                    degrades to the traffic rail placeholder.
// GET  /v1/instances/{name}/logs     SSE of LogLine JSON: backlog, then follow.
// POST /v1/workloads/{name}/scale    body {"replicas": n} — maps ClusterService.Scale
// POST /v1/nodes/{name}/drain        maps ClusterService.Drain (returns when done)
//                                    Cordon has no ClusterService RPC yet, so no
//                                    facade route either. HttpClient.cordon throws.
// GET  /v1/policies/check?from=&to=  → Verdict JSON
// GET  /v1/cluster/topology          → {rev, objects: [...]}
// GET  /healthz                      → 200 when serving
//
// JSON conventions, matching internal/object's user-facing codec:
// - the meta field is spelled "metadata"
// - enums cross in short form: phases as "Ready", "NotReady", "Draining", …
//   and policy actions as "ALLOW"/"DENY"
// - ApplyResult carries action ∈ created|updated|unchanged|deleted, with
//   failures in the error field
// - Service objects arrive decorated with status.vips (node → VIP) and Node
//   objects with status.infra, facade enrichments the UI renders
// ────────────────────────────────────────────────────────────────────────────

import type { KliteClient, Unsubscribe } from './client'
import type {
  ApplyResult,
  Kind,
  KliteObject,
  LogLine,
  TopologySnapshot,
  TrafficEvent,
  Verdict,
  WatchEvent,
} from './types'

const KIND_PATH: Record<Kind, string> = {
  Workload: 'workloads',
  Service: 'services',
  Node: 'nodes',
  NetworkPolicy: 'networkpolicies',
  Instance: 'instances',
}

export class HttpClient implements KliteClient {
  readonly mode = 'http' as const

  private baseUrl: string

  constructor(baseUrl: string) {
    this.baseUrl = baseUrl
  }

  private async json<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`, init)
    if (!res.ok) throw new Error(`${init?.method ?? 'GET'} ${path}: ${res.status}`)
    return (await res.json()) as T
  }

  private sse<T>(
    path: string,
    onEvent: (e: T) => void,
    opts: { onMissing?: () => void } = {},
  ): Unsubscribe {
    // Reconnects ride the protocol: EventSource resends Last-Event-ID, the
    // server resumes from that revision, and it sends {"type":"RESET"} plus a
    // fresh replay only when compaction makes resuming impossible.
    const source = new EventSource(`${this.baseUrl}${path}`)
    const handler = (ev: MessageEvent) => onEvent(JSON.parse(ev.data) as T)
    source.addEventListener('watch', handler)
    source.onmessage = handler
    source.onerror = () => {
      // EventSource retries on its own. An immediate CLOSED usually means 404.
      if (source.readyState === EventSource.CLOSED) opts.onMissing?.()
    }
    return () => source.close()
  }

  apply(yamlText: string): Promise<ApplyResult[]> {
    return this.json('/v1/apply', {
      method: 'POST',
      headers: { 'content-type': 'application/yaml' },
      body: yamlText,
    })
  }

  list(kind: Kind): Promise<KliteObject[]> {
    return this.json(`/v1/${KIND_PATH[kind]}`)
  }

  async get(kind: Kind, name: string): Promise<KliteObject | null> {
    try {
      return await this.json(`/v1/${KIND_PATH[kind]}/${name}`)
    } catch {
      return null
    }
  }

  async remove(kind: Kind, name: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/v1/${KIND_PATH[kind]}/${name}`, { method: 'DELETE' })
    if (!res.ok) throw new Error(`DELETE ${KIND_PATH[kind]}/${name}: ${res.status}`)
  }

  async scale(workload: string, replicas: number): Promise<void> {
    await this.json(`/v1/workloads/${workload}/scale`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ replicas }),
    })
  }

  async drainNode(node: string): Promise<void> {
    await this.json(`/v1/nodes/${node}/drain`, { method: 'POST' })
  }

  async cordon(): Promise<void> {
    throw new Error('cordon has no ClusterService RPC yet — drain instead')
  }

  watch(onEvent: (e: WatchEvent) => void): Unsubscribe {
    return this.sse('/v1/watch', onEvent)
  }

  watchTraffic(onEvent: (e: TrafficEvent) => void): Unsubscribe {
    return this.sse('/v1/traffic', onEvent, {
      // ADR 0024: a 404 means the feed isn't built yet, and the rail's empty state
      // is the degradation, so just stop retrying.
      onMissing: () => console.warn('traffic feed not implemented yet (ADR 0024)'),
    })
  }

  streamLogs(instance: string, onLine: (l: LogLine) => void): Unsubscribe {
    return this.sse(`/v1/instances/${instance}/logs`, onLine)
  }

  policyCheck(from: string, to: string): Promise<Verdict> {
    return this.json(`/v1/policies/check?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`)
  }

  topology(): Promise<TopologySnapshot> {
    return this.json('/v1/cluster/topology')
  }

  async killInstance(name: string): Promise<void> {
    await this.remove('Instance', name)
  }

  async health(): Promise<{ ok: boolean }> {
    try {
      const res = await fetch(`${this.baseUrl}/healthz`)
      return { ok: res.ok }
    } catch {
      return { ok: false }
    }
  }
}
