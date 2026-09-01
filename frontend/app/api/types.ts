// These types mirror api/proto/klite/v1/objects.proto. Field names follow the
// protojson forms the real codec speaks, plus the two user-facing conventions
// internal/object/codec.go applies: the meta field is spelled `metadata`, and
// enums cross the wire in their short forms ("DENY", "Ready"). CONTEXT.md
// vocabulary is binding: Workload, Instance, Service, VIP, Node, Endpoint,
// and never Deployment, Pod, or ClusterIP.

export type Kind = 'Workload' | 'Service' | 'Node' | 'NetworkPolicy' | 'Instance'

export type InstancePhase = 'Pending' | 'Running' | 'Ready' | 'Draining' | 'Failed' | 'Terminating'

export type NodePhase = 'Ready' | 'NotReady' | 'Draining'
export type EndpointState = 'READY' | 'DRAINING'

export interface ObjectMeta {
  name: string
  labels?: Record<string, string>
  uid?: string
  resourceVersion?: number // etcd mod revision, server-set
  createdUnix?: number
}

interface Base<K extends Kind> {
  apiVersion: 'klite/v1'
  kind: K
  metadata: ObjectMeta
}

export interface ContainerSpec {
  name: string
  image: string
  command?: string[]
  args?: string[]
  env?: { name: string; value: string }[]
  ports?: { containerPort: number }[]
  readinessProbe?: { tcpPort: number }
  resources?: { cpus?: string; memory?: string } // cgroup limits only (ADR 0012)
}

// Drain choreography knobs (ADR 0010): integers, like the proto.
export interface DrainSpec {
  drainTimeoutSeconds?: number // default 30 (compressed in the mock)
  terminationGraceSeconds?: number // default 15
}

export interface Workload extends Base<'Workload'> {
  spec: {
    replicas: number
    nodeName?: string // optional pin (ADR 0012)
    template: {
      labels: Record<string, string>
      containers: ContainerSpec[] // v1: exactly one (ADR 0014)
    }
    drain?: DrainSpec
  }
  status?: {
    readyInstances: number
    totalInstances: number
    templateHash?: string // written by the real rollout controller. The mock reconciles replicas only
  }
}

export interface Service extends Base<'Service'> {
  spec: {
    selector: Record<string, string>
    port: number
    targetPort: number
  }
  // Facade enrichment, not stored object state: each Node's own VIP for this
  // Service (ADR 0006). The proto Service carries no status, so klited's
  // topology/watch facade decorates it so the UI can show address ownership.
  status?: { vips: Record<string, string> }
}

export interface NodeObj extends Base<'Node'> {
  spec: {
    maxInstances: number
  }
  status?: {
    phase: NodePhase
    unschedulable?: boolean // cordon lives in status, separate from phase
    lastHeartbeatUnix?: number
    nodeIndex?: number
    instanceCount: number
    infra?: { ip: string; ready: boolean } // facade enrichment
  }
}

export interface PolicyRule {
  from: string // Service name or "*"
  to: string // Service name or "*"
  except?: string[]
}

export interface NetworkPolicy extends Base<'NetworkPolicy'> {
  spec: {
    action: 'ALLOW' | 'DENY'
    rules: PolicyRule[]
  }
}

export interface Instance extends Base<'Instance'> {
  spec: {
    workload: string
    node?: string // empty until the scheduler binds it
    container: ContainerSpec
    templateHash?: string // written by the real rollout controller. The mock reconciles replicas only
    drain?: DrainSpec
  }
  status: {
    phase: InstancePhase
    restarts: number
    instanceIp?: string
    containerId?: string
    message?: string
  }
}

export type KliteObject = Workload | Service | NodeObj | NetworkPolicy | Instance

// A Service selects the instances whose labels carry all its selector pairs.
export function selectorMatches(
  selector: Record<string, string>,
  labels?: Record<string, string>,
): boolean {
  if (!labels) return false
  return Object.entries(selector).every(([k, v]) => labels[k] === v)
}

// Endpoint state is derived, not stored. An Instance is a READY endpoint at
// phase Ready and a DRAINING one at phase Draining, the same rule EDS uses.
export function endpointStateOf(inst: Instance): EndpointState | undefined {
  if (inst.status.phase === 'Ready') return 'READY'
  if (inst.status.phase === 'Draining') return 'DRAINING'
  return undefined
}

// ---------------------------------------------------------------------------
// Watch stream. The gRPC WatchEvent is {type, object, revision}, and the facade
// wraps it in SSE and synthesizes SYNC (after the initial replay) and RESET
// (when resuming past a compaction). Wire shape documented in httpClient.ts.

export type WatchEventType = 'ADDED' | 'MODIFIED' | 'DELETED' | 'SYNC' | 'RESET'

export interface WatchEvent {
  type: WatchEventType
  rev: number // WatchEvent.revision, doubling as the SSE id for Last-Event-ID resume
  kind?: Kind // absent on SYNC / RESET
  object?: KliteObject // full object; DELETED carries the last-known state
}

// ---------------------------------------------------------------------------
// Traffic stream. Mocked today; the real backend adds GET /v1/traffic (SSE,
// fed from Envoy access logs) — ADR 0024, reopening ADR 0015's frozen list.

export interface TrafficEvent {
  id: string
  ts: number
  fromInstance: string
  fromService: string
  toService: string
  viaNode: string // the caller's node — where the enforcing Envoy lives
  verdict: 'allowed' | 'denied'
  reason?: 'policy' | 'no-endpoints'
  matchedRule?: { policy: string; ruleIndex: number; action: 'ALLOW' | 'DENY' }
  toInstance?: string // which instance the call landed on (allowed only)
  latencyMs?: number
}

export interface Verdict {
  allowed: boolean
  reason: 'deny-rule' | 'allow-rule' | 'default-allow' | 'no-allow-match'
  matchedRule?: { policy: string; ruleIndex: number; action: 'ALLOW' | 'DENY' }
}

// Mirrors the proto ApplyResult: action ∈ created|updated|unchanged|deleted,
// with `error` set (and action "error") when a document is rejected.
export interface ApplyResult {
  kind: Kind
  name: string
  action: 'created' | 'updated' | 'unchanged' | 'deleted' | 'error'
  error?: string
}

export interface LogLine {
  ts: number
  line: string
}

export interface TopologySnapshot {
  rev: number
  objects: KliteObject[]
}
