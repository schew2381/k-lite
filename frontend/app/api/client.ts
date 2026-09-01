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
  readonly can: { cordon: boolean; drain: boolean }

  apply(yamlText: string): Promise<ApplyResult[]>
  list(kind: Kind): Promise<KliteObject[]>
  get(kind: Kind, name: string): Promise<KliteObject | null>
  remove(kind: Kind, name: string): Promise<void>

  // First-class RPCs from ClusterService (api/proto/klite/v1/cluster.proto)
  scale(workload: string, replicas: number): Promise<void>
  drainNode(node: string): Promise<void> // cordon + surge-out. The node stays
  cordon(node: string, on: boolean): Promise<void> // Uncordon RPC landed server-side, but no facade route serves either direction yet

  watch(onEvent: (e: WatchEvent) => void): Unsubscribe
  watchTraffic(onEvent: (e: TrafficEvent) => void): Unsubscribe
  streamLogs(instance: string, onLine: (l: LogLine) => void): Unsubscribe

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

export async function createClient(): Promise<KliteClient> {
  const mode = import.meta.env.VITE_KLITE_MODE ?? 'mock'
  if (mode === 'http') {
    const { HttpClient } = await import('./httpClient')
    // '' means same-origin: the embedded case, where klite-facade serves the SPA
    return new HttpClient(import.meta.env.VITE_KLITE_API ?? '')
  }
  const { createMockClient } = await import('./mockClient')
  return createMockClient()
}
