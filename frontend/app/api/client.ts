// Every page talks to a KliteClient and nothing else, so swapping the
// in-browser simulator for the real klited facade is a factory change.

import type {
  ApplyResult,
  Kind,
  KliteObject,
  LogLine,
  PolicyVerdict,
  Topology,
  TrafficEvent,
  WatchEvent,
} from './types'

export type Unsubscribe = () => void

export interface KliteClient {
  readonly mode: 'mock' | 'http'

  // can lists what this client actually serves. The facade grows routes
  // over time, and buttons for missing ones hide instead of throwing.
  // cordon and uncordon differ live: a cordon only happens as the first step
  // of a drain, while uncordon is its own RPC (M8).
  readonly can: { cordon: boolean; uncordon: boolean; drain: boolean }

  apply(yamlText: string): Promise<ApplyResult[]>
  list(kind: Kind): Promise<KliteObject[]>
  get(kind: Kind, name: string): Promise<KliteObject | null>
  remove(kind: Kind, name: string): Promise<void>

  // First-class RPCs from ClusterService (api/proto/klite/v1/cluster.proto)
  scale(workload: string, replicas: number): Promise<void>
  drainNode(node: string): Promise<void> // cordon + surge-out. The node stays
  cordon(node: string, on: boolean): Promise<void> // on=false rides the uncordon route live; on=true is mock-only

  watch(onEvent: (e: WatchEvent) => void): Unsubscribe
  watchTraffic(onEvent: (e: TrafficEvent) => void): Unsubscribe
  streamLogs(instance: string, onLine: (l: LogLine) => void): Unsubscribe

  // Live only: mint a join token plus the klited endpoints a new machine
  // dials. The mock joins nodes instantly, so it has no use for one.
  nodeToken?(): Promise<{ token: string; endpoints: string[] }>
  joinNode?(name: string): Promise<{ ok: boolean; pid: number; log: string }>

  policyCheck(from: string, to: string): Promise<PolicyVerdict>
  topology(): Promise<Topology>
  killInstance(name: string): Promise<void>
  health(): Promise<{ ok: boolean }>
  dispose?(): void

  // Simulation-only controls. The real backend has no equivalent, so the UI
  // renders these affordances only when the field exists.
  chaos?: {
    killNodeAgent(node: string): void
    reviveNodeAgent(node: string): void
    setSpeed(multiplier: number): void // 0 pauses, 1 is demo pace
    speed(): number
  }
}

export async function createClientFor(mode: 'mock' | 'http'): Promise<KliteClient> {
  if (mode === 'http') {
    const { HttpClient } = await import('./httpClient')
    // '' means same-origin: the embedded case, where klite-facade serves the SPA
    return new HttpClient(import.meta.env.VITE_KLITE_API ?? '')
  }
  const { createMockClient } = await import('./mockClient')
  return createMockClient()
}

export async function createClient(): Promise<KliteClient> {
  return createClientFor(import.meta.env.VITE_KLITE_MODE === 'http' ? 'http' : 'mock')
}
