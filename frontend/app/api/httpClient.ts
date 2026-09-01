// HttpClient speaks the REST/SSE facade that internal/facade actually serves
// (ADR 0015 route freeze, ADR 0026 alignment). The facade is a pure
// ClusterService gRPC client, and this header documents its wire behavior:
//
// ── Facade routes (internal/facade/facade.go) ───────────────────────────────
// POST   /api/apply                    body: raw multi-doc YAML
//                                      → protojson ApplyResponse
//                                        {"results":[{kind,name,action,error?}]}
// GET    /api/{kind}                   → {"items":[<codec JSON object>...]}
//                                      kind is the lowercase plural; add
//                                      ?name= for a single object
// DELETE /api/{kind}/{name}            → protojson DeleteResponse (the same
//                                      results shape). Deleting a node runs
//                                      the drain choreography first (ADR 0010)
// GET    /api/watch                    SSE. event: ADDED|MODIFIED|DELETED,
//                                      data: protojson WatchEvent
//                                      {type,object,revision}. Changes only —
//                                      no replay and no resume, so bootstrap
//                                      is list-then-watch and a reconnect
//                                      means RESET plus a fresh re-list.
// GET    /api/instances/{name}/logs    chunked text/plain, ?follow=1&tail=N.
//                                      The facade walks klited endpoints to
//                                      find the one holding the agent stream.
// GET    /api/policycheck?from=&to=    → {"available":false} until klited
//                                      implements the RPC, then
//                                      {"available":true,"allowed":...,
//                                       "matchedPolicy":...,"reason":...}
// GET    /api/topology                 → the composed graph (Topology type)
//
// Not served yet: scale and drain routes (the RPCs exist, and scale rides
// apply below until a route lands), cordon (no RPC at all), and GET /api/traffic
// (ADR 0024's feed). watchTraffic degrades to the rail's waiting state.
// ─────────────────────────────────────────────────────────────────────────────

import type { KliteClient, Unsubscribe } from './client'
import { decodeObject, KIND_PATH } from './decode'
import type {
  ApplyResult,
  Kind,
  KliteObject,
  LogLine,
  PolicyVerdict,
  Topology,
  WatchEvent,
} from './types'

interface WireResults {
  results?: { kind?: string; name?: string; action?: string; error?: string }[]
}

function toApplyResults(wire: WireResults): ApplyResult[] {
  return (wire.results ?? []).map((r) => ({
    kind: (r.kind ?? 'Workload') as Kind,
    name: r.name ?? '',
    action: (r.error ? 'error' : (r.action ?? 'unchanged')) as ApplyResult['action'],
    ...(r.error && { error: r.error }),
  }))
}

export class HttpClient implements KliteClient {
  readonly mode = 'http' as const

  private baseUrl: string

  constructor(baseUrl: string) {
    this.baseUrl = baseUrl
  }

  private async json<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await fetch(`${this.baseUrl}${path}`, init)
    if (!res.ok) {
      const body = (await res.json().catch(() => null)) as { error?: string } | null
      throw new Error(body?.error ?? `${init?.method ?? 'GET'} ${path}: ${res.status}`)
    }
    return (await res.json()) as T
  }

  async apply(yamlText: string): Promise<ApplyResult[]> {
    const wire = await this.json<WireResults>('/api/apply', {
      method: 'POST',
      headers: { 'content-type': 'application/yaml' },
      body: yamlText,
    })
    return toApplyResults(wire)
  }

  async list(kind: Kind): Promise<KliteObject[]> {
    const wire = await this.json<{ items: unknown[] }>(`/api/${KIND_PATH[kind]}`)
    return (wire.items ?? []).map(decodeObject).filter((o): o is KliteObject => o !== null)
  }

  async get(kind: Kind, name: string): Promise<KliteObject | null> {
    try {
      const wire = await this.json<{ items: unknown[] }>(
        `/api/${KIND_PATH[kind]}?name=${encodeURIComponent(name)}`,
      )
      return decodeObject(wire.items?.[0]) ?? null
    } catch {
      return null
    }
  }

  async remove(kind: Kind, name: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/api/${KIND_PATH[kind]}/${name}`, { method: 'DELETE' })
    if (!res.ok) {
      const body = (await res.json().catch(() => null)) as { error?: string } | null
      throw new Error(body?.error ?? `DELETE ${KIND_PATH[kind]}/${name}: ${res.status}`)
    }
  }

  // The Scale RPC exists but has no facade route yet, so scaling rides the
  // same channel the CLI's edit-and-apply does. Once a route lands, this
  // collapses to a single POST.
  async scale(workload: string, replicas: number): Promise<void> {
    const wl = await this.get('Workload', workload)
    if (wl?.kind !== 'Workload') throw new Error(`workload ${workload} not found`)
    const doc = {
      apiVersion: wl.apiVersion,
      kind: wl.kind,
      metadata: { name: wl.metadata.name, labels: wl.metadata.labels },
      spec: { ...wl.spec, replicas },
    }
    const results = await this.apply(JSON.stringify(doc))
    const failed = results.find((r) => r.action === 'error')
    if (failed) throw new Error(failed.error ?? 'scale rejected')
  }

  async drainNode(): Promise<void> {
    throw new Error('the facade has no drain route yet — use "Drain & remove" instead')
  }

  async cordon(): Promise<void> {
    throw new Error('cordon has no ClusterService RPC yet — drain instead')
  }

  // list-then-watch: the Watch RPC sends changes only, so the bootstrap lists
  // every kind, replays the results as ADDED events plus a SYNC, then follows
  // the SSE stream. A
  // dropped stream loses continuity (there is no resume), so a reconnect
  // RESETs the store and bootstraps again.
  watch(onEvent: (e: WatchEvent) => void): Unsubscribe {
    let closed = false
    let source: EventSource | null = null

    const bootstrap = async () => {
      const kinds = Object.keys(KIND_PATH) as Kind[]
      const lists = await Promise.all(kinds.map((k) => this.list(k)))
      if (closed) return
      let rev = 0
      lists.forEach((objects, i) => {
        for (const object of objects) {
          rev = Math.max(rev, object.metadata.resourceVersion ?? 0)
          onEvent({ type: 'ADDED', rev, kind: kinds[i], object })
        }
      })
      onEvent({ type: 'SYNC', rev })
    }

    const connect = () => {
      if (closed) return
      source = new EventSource(`${this.baseUrl}/api/watch`)
      const deliver = (type: 'ADDED' | 'MODIFIED' | 'DELETED') => (ev: MessageEvent) => {
        const wire = JSON.parse(ev.data) as { object?: unknown; revision?: string | number }
        const object = decodeObject(wire.object)
        if (!object) return
        onEvent({ type, rev: Number(wire.revision ?? 0), kind: object.kind, object })
      }
      source.addEventListener('ADDED', deliver('ADDED'))
      source.addEventListener('MODIFIED', deliver('MODIFIED'))
      source.addEventListener('DELETED', deliver('DELETED'))
      source.onerror = () => {
        // whatever happened during the outage is gone, so start over
        source?.close()
        if (closed) return
        onEvent({ type: 'RESET', rev: 0 })
        window.setTimeout(() => {
          if (closed) return
          bootstrap()
            .then(connect)
            .catch(() => window.setTimeout(connect, 3000))
        }, 1000)
      }
    }

    bootstrap()
      .then(connect)
      .catch(() => {
        if (!closed)
          window.setTimeout(
            () =>
              bootstrap()
                .then(connect)
                .catch(() => {}),
            3000,
          )
      })
    return () => {
      closed = true
      source?.close()
    }
  }

  watchTraffic(): Unsubscribe {
    // ADR 0024 records GET /api/traffic, but nothing serves it yet. The rail
    // keeps its waiting message until the feed exists.
    console.warn('traffic feed not implemented yet (ADR 0024)')
    return () => {}
  }

  // Logs arrive as chunked text/plain, one runtime log line per line.
  streamLogs(instance: string, onLine: (l: LogLine) => void): Unsubscribe {
    const controller = new AbortController()
    const url = `${this.baseUrl}/api/instances/${instance}/logs?follow=1&tail=200`
    ;(async () => {
      const res = await fetch(url, { signal: controller.signal })
      if (!res.ok || !res.body) return
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
      let buffer = ''
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buffer += value
        const lines = buffer.split('\n')
        buffer = lines.pop() ?? ''
        for (const line of lines) {
          if (line.length > 0) onLine({ ts: Date.now(), line })
        }
      }
      if (buffer.length > 0) onLine({ ts: Date.now(), line: buffer })
    })().catch(() => {
      // aborted by unsubscribe, or the instance is gone — the pane just stops
    })
    return () => controller.abort()
  }

  async policyCheck(from: string, to: string): Promise<PolicyVerdict> {
    return this.json<PolicyVerdict>(
      `/api/policycheck?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`,
    )
  }

  topology(): Promise<Topology> {
    return this.json<Topology>('/api/topology')
  }

  async killInstance(name: string): Promise<void> {
    await this.remove('Instance', name)
  }

  // The facade has no health route, so a list answering is the signal.
  async health(): Promise<{ ok: boolean }> {
    try {
      const res = await fetch(`${this.baseUrl}/api/nodes`)
      return { ok: res.ok }
    } catch {
      return { ok: false }
    }
  }
}
