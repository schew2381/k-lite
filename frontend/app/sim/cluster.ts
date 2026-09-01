// Cluster is the in-browser control plane. It imports no React and reads no
// wall clock: time only passes through advance(dtSimMs), so tests drive it
// deterministically and the UI's speed slider is a single multiplier.
//
// Semantics come from the ADRs, not convenience:
//   0009 istio-lite policies (sim/policy.ts, evaluated at call time)
//   0010 surge-first drain — capacity never dips and DRAINING takes no new picks
//   0011 agent-owned restarts with honest counts
//   0012 spread scheduling (sim/scheduler.ts)

import {
  type ApplyResult,
  endpointStateOf,
  type Instance,
  type Kind,
  type KliteObject,
  type NetworkPolicy,
  type NodeObj,
  type Service,
  selectorMatches,
  type TrafficEvent,
  type Verdict,
  type WatchEvent,
  type Workload,
} from '@/api/types'
import { Ipam } from './ipam'
import { LogStore } from './logs'
import { evaluate } from './policy'
import { pickNode } from './scheduler'
import { DEFAULT_TIMINGS, type SimTimings } from './timings'

type WatchCb = (e: WatchEvent) => void
type TrafficCb = (e: TrafficEvent) => void

interface Task {
  due: number
  key: string // one pending task per key. Re-adding replaces it
  run: () => void
}

interface AgentState {
  alive: boolean
  lastBeat: number
  infraReadyAt: number
}

interface InstanceMeta {
  evacuating: boolean // node is draining and a surge replacement must come first
  scalingDown: boolean // drain-out with no replacement owed
  backoffCount: number
  lastHealthyAt: number
}

function mulberry32(seed: number) {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) | 0
    let t = Math.imul(a ^ (a >>> 15), 1 | a)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const clone = <T>(o: T): T => structuredClone(o)

const secondsToMs = (s?: number) => (s && s > 0 ? s * 1000 : undefined)

export class Cluster {
  readonly logs = new LogStore()

  private now = 0
  private rev = 0
  private objects: Record<Kind, Map<string, KliteObject>> = {
    Workload: new Map(),
    Service: new Map(),
    Node: new Map(),
    NetworkPolicy: new Map(),
    Instance: new Map(),
  }
  private tasks: Task[] = []
  private agents = new Map<string, AgentState>()
  private meta = new Map<string, InstanceMeta>()
  private seq = new Map<string, number>()
  private removingNodes = new Set<string>()
  private rrCursor = new Map<string, number>()
  private targetCursor = new Map<string, number>()
  private lastTraffic = 0
  private lastSweepBeat = 0
  private watchers = new Set<WatchCb>()
  private trafficWatchers = new Set<TrafficCb>()
  private ipam = new Ipam()
  private rand: () => number
  private trafficSeq = 0

  private t: SimTimings

  constructor(seed: KliteObject[] = [], timings: SimTimings = DEFAULT_TIMINGS, randomSeed = 42) {
    this.t = timings
    this.rand = mulberry32(randomSeed)
    this.applyObjects(seed)
  }

  // ------------------------------------------------------------------ time

  advance(dtMs: number) {
    if (dtMs <= 0) return
    this.now += dtMs

    // 1. due tasks. Tasks scheduled by a running task keep their insertion
    // order until the next advance, so a same-tick follow-up waits one tick.
    this.tasks.sort((a, b) => a.due - b.due)
    while (this.tasks.length && this.tasks[0].due <= this.now) {
      const task = this.tasks.shift()
      task?.run()
    }

    // 2. level-based controller sweep (cheap at this scale, self-healing)
    if (this.now - this.lastSweepBeat >= this.t.heartbeatMs) {
      this.lastSweepBeat = this.now
      this.nodeLifecycle()
    }
    this.reconcileWorkloads()
    this.schedule()

    // 3. traffic generator
    if (this.now - this.lastTraffic >= this.t.trafficPeriodMs) {
      this.lastTraffic = this.now
      this.generateTraffic()
    }
  }

  private after(ms: number, key: string, run: () => void) {
    this.tasks = this.tasks.filter((t) => t.key !== key)
    this.tasks.push({ due: this.now + ms, key, run })
  }

  // Exact-key only: prefix matching once let `lifecycle:b-1` cancel b-10's
  // pending termination and strand it in Terminating forever.
  private cancel(key: string) {
    this.tasks = this.tasks.filter((t) => t.key !== key)
  }

  // ---------------------------------------------------------------- events

  private emit(type: 'ADDED' | 'MODIFIED' | 'DELETED', obj: KliteObject) {
    this.rev++
    obj.metadata.resourceVersion = this.rev // etcd's mod revision, server-set
    const e: WatchEvent = { type, rev: this.rev, kind: obj.kind, object: clone(obj) }
    for (const cb of this.watchers) cb(e)
  }

  subscribe(cb: WatchCb): () => void {
    for (const kind of Object.keys(this.objects) as Kind[]) {
      for (const obj of this.objects[kind].values()) {
        cb({ type: 'ADDED', rev: this.rev, kind, object: clone(obj) })
      }
    }
    cb({ type: 'SYNC', rev: this.rev })
    this.watchers.add(cb)
    return () => this.watchers.delete(cb)
  }

  subscribeTraffic(cb: TrafficCb): () => void {
    this.trafficWatchers.add(cb)
    return () => this.trafficWatchers.delete(cb)
  }

  // ----------------------------------------------------------------- reads

  list(kind: Kind): KliteObject[] {
    return [...this.objects[kind].values()].map(clone)
  }

  get(kind: Kind, name: string): KliteObject | null {
    const o = this.objects[kind].get(name)
    return o ? clone(o) : null
  }

  currentRev() {
    return this.rev
  }

  policyCheck(from: string, to: string): Verdict {
    return evaluate([...this.objects.NetworkPolicy.values()] as NetworkPolicy[], from, to)
  }

  // ---------------------------------------------------------------- writes

  applyObjects(objs: KliteObject[]): ApplyResult[] {
    const results: ApplyResult[] = []
    for (const obj of objs) {
      const { kind } = obj
      const name = obj.metadata.name
      if (kind === 'Instance') {
        results.push({
          kind,
          name,
          action: 'error',
          error: 'Instances are server-materialized; apply a Workload instead',
        })
        continue
      }
      const existing = this.objects[kind].get(name)
      if (
        existing &&
        JSON.stringify(existing.metadata.labels) === JSON.stringify(obj.metadata.labels) &&
        JSON.stringify((existing as { spec: unknown }).spec) ===
          JSON.stringify((obj as { spec: unknown }).spec)
      ) {
        results.push({ kind, name, action: 'unchanged' })
        continue
      }
      const stored = clone(obj)
      stored.metadata.createdUnix = existing?.metadata.createdUnix ?? this.simUnix()
      if (kind === 'Node') {
        const n = stored as NodeObj
        const prev = existing as NodeObj | undefined
        n.status = prev?.status ?? { phase: 'NotReady', instanceCount: 0 }
        if (!this.agents.has(name)) {
          this.agents.set(name, {
            alive: true,
            lastBeat: this.now,
            infraReadyAt: this.now + this.t.infraStartMs,
          })
          this.after(this.t.infraStartMs, `infra:${name}`, () => this.infraUp(name))
        }
      }
      if (kind === 'Service') {
        const s = stored as Service
        const prev = existing as Service | undefined
        s.status = prev?.status ?? { vips: {} }
        for (const node of this.objects.Node.keys()) {
          s.status.vips[node] ??= this.ipam.vip()
        }
      }
      this.objects[kind].set(name, stored)
      this.emit(existing ? 'MODIFIED' : 'ADDED', stored)
      if (kind === 'Node' && !existing) this.assignVipsForNode(name)
      results.push({ kind, name, action: existing ? 'updated' : 'created' })
    }
    return results
  }

  remove(kind: Kind, name: string) {
    const obj = this.objects[kind].get(name)
    if (!obj) return
    if (kind === 'Node') {
      this.removingNodes.add(name)
      this.cordonAndDrain(name)
      return // deleted once empty, in nodeLifecycle
    }
    if (kind === 'Instance') {
      this.killInstance(name)
      return
    }
    this.objects[kind].delete(name)
    this.emit('DELETED', obj)
    // Instances of a deleted Workload drain out via reconcile. A deleted
    // Service stops resolving (no-endpoints), and policies flip verdicts.
  }

  killInstance(name: string) {
    const inst = this.objects.Instance.get(name) as Instance | undefined
    if (!inst) return
    if (inst.status.phase !== 'Running' && inst.status.phase !== 'Ready') return
    const m = this.metaOf(name)
    if (this.now - m.lastHealthyAt > this.t.backoffResetAfterMs) m.backoffCount = 0
    inst.status.phase = 'Failed'
    inst.status.message = 'killed'
    this.emit('MODIFIED', inst)
    this.logs.push(name, this.now, 'container exited (137)')
    const backoff = this.t.restartBackoffMs[Math.min(m.backoffCount, this.t.restartBackoffMs.length - 1)]
    m.backoffCount++
    this.after(backoff, `restart:${name}`, () => this.restart(name))
  }

  // Uncordon reopens scheduling but does not recall instances already
  // evacuating: a drain in flight finishes, matching kubectl's semantics.
  cordon(name: string, on: boolean) {
    const node = this.objects.Node.get(name) as NodeObj | undefined
    if (!node?.status) return
    node.status.unschedulable = on
    this.emit('MODIFIED', node)
  }

  drainNode(name: string) {
    this.cordonAndDrain(name)
  }

  killNodeAgent(name: string) {
    const a = this.agents.get(name)
    if (a) a.alive = false
  }

  reviveNodeAgent(name: string) {
    const a = this.agents.get(name)
    if (!a) return
    a.alive = true
    a.lastBeat = this.now
  }

  // ------------------------------------------------------------ controllers

  private infraUp(name: string) {
    const node = this.objects.Node.get(name) as NodeObj | undefined
    if (!node) return
    node.status = {
      ...node.status,
      phase: 'Ready',
      instanceCount: this.instancesOn(name).length,
      infra: { ip: this.ipam.infraIp(name), ready: true },
    }
    this.emit('MODIFIED', node)
  }

  private assignVipsForNode(name: string) {
    for (const svc of this.objects.Service.values() as Iterable<Service>) {
      svc.status ??= { vips: {} }
      if (!svc.status.vips[name]) {
        svc.status.vips[name] = this.ipam.vip()
        this.emit('MODIFIED', svc)
      }
    }
  }

  private cordonAndDrain(name: string) {
    const node = this.objects.Node.get(name) as NodeObj | undefined
    if (!node) return
    if (!node.status) return
    node.status.unschedulable = true
    node.status.phase = 'Draining'
    this.emit('MODIFIED', node)
    for (const inst of this.instancesOn(name)) {
      this.metaOf(inst.metadata.name).evacuating = true
    }
  }

  private nodeLifecycle() {
    for (const [name, agent] of this.agents) {
      const node = this.objects.Node.get(name) as NodeObj | undefined
      if (!node?.status) continue

      if (agent.alive) {
        agent.lastBeat = this.now
        if (node.status.phase === 'NotReady') {
          // agent came back before (or after) the grace: node serves again
          node.status.phase = node.status.unschedulable ? 'Draining' : 'Ready'
          this.emit('MODIFIED', node)
          this.cancel(`reschedule:${name}`)
          for (const inst of this.instancesOn(name)) {
            if (inst.status.message === 'node-lost') this.restart(inst.metadata.name)
          }
        }
      } else if (
        node.status.phase !== 'NotReady' &&
        this.now - agent.lastBeat >= this.t.heartbeatMs * this.t.missedHeartbeatsForNotReady
      ) {
        node.status.phase = 'NotReady'
        this.emit('MODIFIED', node)
        for (const inst of this.instancesOn(name)) {
          if (inst.status.phase === 'Draining' || inst.status.phase === 'Terminating') {
            // already on its way out and its container is gone with the node
            this.deleteInstance(inst.metadata.name)
            continue
          }
          inst.status.phase = 'Failed'
          inst.status.message = 'node-lost'
          this.cancel(`restart:${inst.metadata.name}`)
          this.cancel(`lifecycle:${inst.metadata.name}`)
          this.emit('MODIFIED', inst)
        }
        this.after(this.t.rescheduleGraceMs, `reschedule:${name}`, () => {
          const n = this.objects.Node.get(name) as NodeObj | undefined
          if (n?.status?.phase !== 'NotReady') return
          // give up on the node's instances. Reconcile replaces them elsewhere
          for (const inst of this.instancesOn(name)) this.deleteInstance(inst.metadata.name)
        })
      }

      // Removal completes when nothing runs there anymore, whatever the
      // phase: a node whose agent died mid-removal still leaves once the
      // reschedule grace clears its instances.
      if (this.instancesOn(name).length === 0) {
        if (this.removingNodes.has(name)) {
          this.removingNodes.delete(name)
          this.agents.delete(name)
          this.objects.Node.delete(name)
          this.emit('DELETED', node)
          for (const svc of this.objects.Service.values() as Iterable<Service>) {
            if (svc.status?.vips[name]) {
              delete svc.status.vips[name]
              this.emit('MODIFIED', svc)
            }
          }
          continue // nothing below may emit for this node: MODIFIED after DELETED resurrects it downstream
        }
        if (node.status.phase === 'Draining') {
          node.status.phase = 'Ready' // drained but still cordoned
          this.emit('MODIFIED', node)
        }
      }

      const count = this.instancesOn(name).length
      if (node.status.instanceCount !== count) {
        node.status.instanceCount = count
        this.emit('MODIFIED', node)
      }
    }
  }

  private reconcileWorkloads() {
    for (const wl of this.objects.Workload.values() as Iterable<Workload>) {
      const name = wl.metadata.name
      const all = this.instancesOf(name)
      const serving = all.filter((i) => {
        const m = this.metaOf(i.metadata.name)
        // node-lost instances still count: replacements wait for the
        // reschedule grace to delete them, so an agent blip causes no churn
        return (
          !m.evacuating &&
          !m.scalingDown &&
          i.status.phase !== 'Terminating' &&
          i.status.phase !== 'Draining' // already on its way out — never counts toward replicas
        )
      })

      // want more: create Pending records (declare-then-converge is visible)
      for (let missing = wl.spec.replicas - serving.length; missing > 0; missing--) {
        this.createInstance(wl)
      }

      // want fewer: drain out the newest extras
      const extras = serving.length - wl.spec.replicas
      if (extras > 0) {
        const victims = [...serving].sort((a, b) => seqOf(b) - seqOf(a)).slice(0, extras)
        for (const v of victims) {
          this.metaOf(v.metadata.name).scalingDown = true
          this.startDrainOut(v.metadata.name, wl)
        }
      }

      // surge-first evacuation: an evacuating instance may drain only once a
      // replacement is Ready — capacity never dips (ADR 0010)
      const readyServing = serving.filter((i) => i.status.phase === 'Ready').length
      if (readyServing >= wl.spec.replicas) {
        const evacuee = all.find(
          (i) => this.metaOf(i.metadata.name).evacuating && i.status.phase === 'Ready',
        )
        if (evacuee) this.startDrainOut(evacuee.metadata.name, wl)
      }
    }

    // orphans: instances whose Workload is gone drain out immediately
    for (const inst of this.objects.Instance.values() as Iterable<Instance>) {
      if (!this.objects.Workload.has(inst.spec.workload)) {
        const m = this.metaOf(inst.metadata.name)
        if (!m.scalingDown && inst.status.phase !== 'Terminating') {
          m.scalingDown = true
          this.startDrainOut(inst.metadata.name, undefined)
        }
      }
    }
  }

  private schedule() {
    for (const inst of this.objects.Instance.values() as Iterable<Instance>) {
      if (inst.spec.node || inst.status.phase !== 'Pending') continue
      const nodes = [...this.objects.Node.values()].map((n) => ({
        node: n as NodeObj,
        infraReady: (n as NodeObj).status?.infra?.ready ?? false,
      }))
      const placed = [...this.objects.Instance.values()] as Instance[]
      const wl = this.objects.Workload.get(inst.spec.workload) as Workload | undefined
      const chosen = pickNode(wl?.spec.nodeName, nodes, placed)
      if (!chosen) continue
      inst.spec.node = chosen // binding is one field write
      this.emit('MODIFIED', inst)
      this.after(this.t.containerStartMs, `lifecycle:${inst.metadata.name}`, () =>
        this.containerStarted(inst.metadata.name),
      )
    }
  }

  // ------------------------------------------------------------- lifecycle

  private createInstance(wl: Workload) {
    const n = (this.seq.get(wl.metadata.name) ?? 0) + 1
    this.seq.set(wl.metadata.name, n)
    const name = `${wl.metadata.name}-${n}`
    const inst: Instance = {
      apiVersion: 'klite/v1',
      kind: 'Instance',
      metadata: {
        name,
        labels: { ...wl.spec.template.labels },
        createdUnix: this.simUnix(),
      },
      spec: {
        workload: wl.metadata.name,
        container: clone(wl.spec.template.containers[0]),
      },
      status: { phase: 'Pending', restarts: 0 },
    }
    this.meta.set(name, {
      evacuating: false,
      scalingDown: false,
      backoffCount: 0,
      lastHealthyAt: this.now,
    })
    this.objects.Instance.set(name, inst)
    this.emit('ADDED', inst)
  }

  private containerStarted(name: string) {
    const inst = this.objects.Instance.get(name) as Instance | undefined
    if (inst?.status.phase !== 'Pending') return
    inst.status.phase = 'Running'
    inst.status.instanceIp ??= this.ipam.instanceIp()
    inst.status.message = undefined
    this.emit('MODIFIED', inst)
    this.logs.push(name, this.now, `started ${inst.spec.container.image} on ${inst.spec.node}`)
    const probe = inst.spec.container.readinessProbe ? this.t.probeMs : 0
    this.after(probe, `lifecycle:${name}`, () => this.becomeReady(name))
  }

  private becomeReady(name: string) {
    const inst = this.objects.Instance.get(name) as Instance | undefined
    if (inst?.status.phase !== 'Running') return
    inst.status.phase = 'Ready'
    this.metaOf(name).lastHealthyAt = this.now
    this.emit('MODIFIED', inst)
    if (inst.spec.container.readinessProbe) {
      this.logs.push(
        name,
        this.now,
        `readiness probe passed on :${inst.spec.container.readinessProbe.tcpPort}`,
      )
    }
  }

  private restart(name: string) {
    const inst = this.objects.Instance.get(name) as Instance | undefined
    if (!inst || inst.status.phase === 'Terminating') return
    const m = this.metaOf(name)
    if (m.scalingDown || m.evacuating) {
      // it was leaving when it died, and resurrecting it would mint an extra instance
      this.startDrainOut(name, this.objects.Workload.get(inst.spec.workload) as Workload | undefined)
      return
    }
    const node = this.objects.Node.get(inst.spec.node ?? '') as NodeObj | undefined
    if (!node || node.status?.phase === 'NotReady') return // wait for revive or reschedule
    inst.status.phase = 'Running'
    inst.status.restarts += 1
    inst.status.message = undefined
    this.emit('MODIFIED', inst)
    this.logs.push(name, this.now, `restarted by agent (restart #${inst.status.restarts})`)
    const probe = inst.spec.container.readinessProbe ? this.t.probeMs : 0
    this.after(probe, `lifecycle:${name}`, () => this.becomeReady(name))
  }

  private startDrainOut(name: string, wl: Workload | undefined) {
    const inst = this.objects.Instance.get(name) as Instance | undefined
    if (!inst) return
    if (inst.status.phase === 'Draining' || inst.status.phase === 'Terminating') return
    const m = this.metaOf(name)
    m.evacuating = false // the decision is made, so it no longer waits for a replacement
    inst.status.phase = 'Draining' // Draining endpoints take no new connections (EDS semantics)
    this.emit('MODIFIED', inst)
    const drainMs = secondsToMs(wl?.spec.drain?.drainTimeoutSeconds) ?? this.t.drainTimeoutMs
    const graceMs = secondsToMs(wl?.spec.drain?.terminationGraceSeconds) ?? this.t.terminationGraceMs
    this.after(drainMs, `lifecycle:${name}`, () => {
      const i = this.objects.Instance.get(name) as Instance | undefined
      if (!i) return
      i.status.phase = 'Terminating'
      this.emit('MODIFIED', i)
      this.after(graceMs, `lifecycle:${name}`, () => this.deleteInstance(name))
    })
  }

  private deleteInstance(name: string) {
    const inst = this.objects.Instance.get(name) as Instance | undefined
    if (!inst) return
    this.objects.Instance.delete(name)
    this.meta.delete(name)
    this.cancel(`restart:${name}`)
    this.cancel(`lifecycle:${name}`)
    this.logs.drop(name)
    this.emit('DELETED', inst)
  }

  // -------------------------------------------------------------- traffic

  // One traced call per beat: a random Ready instance dials the next service
  // in its own rotation (never its own). Random enough to feel alive, cyclic
  // enough that every pair shows up on a schedule.
  private generateTraffic() {
    const services = [...this.objects.Service.keys()].sort()
    if (services.length < 2) return
    const pool = ([...this.objects.Instance.values()] as Instance[]).filter(
      (i) => i.status.phase === 'Ready' && i.spec.node,
    )
    if (pool.length === 0) return
    const inst = pool[Math.floor(this.rand() * pool.length)]
    const fromService = this.serviceSelecting(inst)?.metadata.name
    if (!fromService) return
    const others = services.filter((s) => s !== fromService)
    if (others.length === 0) return
    const cursor = this.targetCursor.get(inst.metadata.name) ?? 0
    this.targetCursor.set(inst.metadata.name, cursor + 1)
    this.call(inst, fromService, others[cursor % others.length])
  }

  private call(from: Instance, fromService: string, toService: string) {
    const viaNode = from.spec.node as string
    const policies = [...this.objects.NetworkPolicy.values()] as NetworkPolicy[]
    const verdict = evaluate(policies, fromService, toService)
    const base = {
      id: `t${++this.trafficSeq}`,
      ts: this.now,
      fromInstance: from.metadata.name,
      fromService,
      toService,
      viaNode,
    }

    if (!verdict.allowed) {
      this.emitTraffic({
        ...base,
        verdict: 'denied',
        reason: 'policy',
        matchedRule: verdict.matchedRule,
      })
      const by = verdict.matchedRule ? ` by ${verdict.matchedRule.policy}` : ''
      this.logs.push(from.metadata.name, this.now, `${toService} => blocked${by} (connection reset)`)
      return
    }

    const endpoints = this.readyEndpoints(toService)
    if (endpoints.length === 0) {
      this.emitTraffic({ ...base, verdict: 'denied', reason: 'no-endpoints' })
      this.logs.push(from.metadata.name, this.now, `${toService} => FAILED (no endpoints)`)
      return
    }

    // Envoy keeps per-node LB state: round-robin cursor per (node, service)
    const key = `${viaNode}/${toService}`
    const cursor = this.rrCursor.get(key) ?? 0
    this.rrCursor.set(key, cursor + 1)
    const to = endpoints[cursor % endpoints.length]
    const latencyMs = Math.round((2 + this.rand() * 8) * 10) / 10
    this.emitTraffic({
      ...base,
      verdict: 'allowed',
      matchedRule: verdict.matchedRule,
      toInstance: to.metadata.name,
      latencyMs,
    })
    this.logs.push(
      from.metadata.name,
      this.now,
      `${toService} => Hostname: ${to.metadata.name} (${latencyMs}ms)`,
    )
    this.logs.push(
      to.metadata.name,
      this.now,
      `GET / from ${from.status.instanceIp ?? '?'} (${fromService}) 200 OK`,
    )
  }

  private emitTraffic(e: TrafficEvent) {
    for (const cb of this.trafficWatchers) cb(e)
  }

  private readyEndpoints(serviceName: string): Instance[] {
    const svc = this.objects.Service.get(serviceName) as Service | undefined
    if (!svc) return []
    return ([...this.objects.Instance.values()] as Instance[])
      .filter(
        (i) => endpointStateOf(i) === 'READY' && selectorMatches(svc.spec.selector, i.metadata.labels),
      )
      .sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
  }

  private serviceSelecting(inst: Instance): Service | undefined {
    for (const svc of this.objects.Service.values() as Iterable<Service>) {
      if (selectorMatches(svc.spec.selector, inst.metadata.labels)) return svc
    }
    return undefined
  }

  // --------------------------------------------------------------- helpers

  private instancesOn(node: string): Instance[] {
    return ([...this.objects.Instance.values()] as Instance[]).filter((i) => i.spec.node === node)
  }

  private instancesOf(workload: string): Instance[] {
    return ([...this.objects.Instance.values()] as Instance[]).filter(
      (i) => i.spec.workload === workload,
    )
  }

  // The sim's own clock, anchored to a fixed epoch so runs are reproducible.
  private simUnix(): number {
    return 1_756_600_000 + Math.floor(this.now / 1000)
  }

  private metaOf(name: string): InstanceMeta {
    let m = this.meta.get(name)
    if (!m) {
      m = { evacuating: false, scalingDown: false, backoffCount: 0, lastHealthyAt: this.now }
      this.meta.set(name, m)
    }
    return m
  }
}

// b-10 outranks b-9 as "newest": compare the numeric seq, not the string.
function seqOf(inst: Instance): number {
  return Number(inst.metadata.name.slice(inst.metadata.name.lastIndexOf('-') + 1)) || 0
}
