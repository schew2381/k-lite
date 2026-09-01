// The dot layer has two flows (ADR 0027). Traced flow runs one request at a
// time at the mock's teaching pace, with long holds, a two-line label (route
// above, numbered step below), and the trace panel stepping in sync. Live
// flow, for a real cluster, flies every request over the same full path fast
// and concurrent with only its route label, and the panel just keeps the
// latest call.
// The implementation is one SVG and one requestAnimationFrame loop, with no
// React render per frame. Anchors come from the layout ref each frame, so a
// mid-flight layout shift bends the path instead of breaking it.

import { useEffect, useRef } from 'react'
import type { Layout, Point } from '@/layout/layout'
import { useClient } from '@/lib/client-context'
import { clusterStore } from '@/store/store'
import { traceStore } from '@/store/traceStore'
import type { FlowMode } from '@/topo/flow'
import { anchorIdFor, buildTrace, type Trace, type TraceStep } from '@/topo/trace'

// Per-flow pacing in ms. Traced is readable and live is honest about speed.
interface Pace {
  pause: number
  stepTravel: number
  settle: number
  travel: number
  shortHop: number
}
const PACE: Record<FlowMode, Pace> = {
  traced: { pause: 2600, stepTravel: 1500, settle: 900, travel: 900, shortHop: 350 },
  live: { pause: 200, stepTravel: 360, settle: 0, travel: 360, shortHop: 160 },
}

const LIVE_FLIGHT_CAP = 24 // beyond this, extra calls stay rail-only
const SVGNS = 'http://www.w3.org/2000/svg'

type Phase =
  | { kind: 'pause'; anchorId: string; stepIndex: number; short: string; quick?: boolean }
  | {
      kind: 'travel'
      fromId: string
      toId: string
      label: string
      stepIndex?: number
      zoom?: boolean // the story's headline leg crosses fast, then holds
    }
  | { kind: 'settle'; anchorId: string; hold?: boolean } // rest after landing, label unchanged

interface Flight {
  trace: Trace
  phases: Phase[]
  phase: number
  phaseStart: number
  color: string
  denyAtEnd: boolean
  pulseTarget?: string
  dot: SVGCircleElement
  route: SVGTextElement // "a→b", pinned above the dot for the whole flight
  step: SVGTextElement | null // "3. dial 10.44.64.7:8080", traced flow only
}

// trapezoidal velocity: ramp to full speed over the first 8% of the flight,
// cruise, brake over the last 8% — quick off the mark, no floaty landing
const RAMP = 0.08
function easeTravel(k: number): number {
  const s = 1 / (1 - RAMP)
  if (k < RAMP) return ((k * k) / (2 * RAMP)) * s
  if (k <= 1 - RAMP) return (RAMP / 2 + (k - RAMP)) * s
  return (1 - RAMP - ((1 - k) * (1 - k)) / (2 * RAMP)) * s
}

// The bow grows with distance: a cross-board flight arcs visibly, while a
// hop between neighboring sub-boxes stays flat enough to read as straight.
const BOW_PER_PX = 0.09
const BOW_MAX = 52

function bowFor(a: Point, b: Point): number {
  return Math.min(Math.hypot(b.x - a.x, b.y - a.y) * BOW_PER_PX, BOW_MAX)
}

function arc(a: Point, b: Point, t: number, bow: number): Point {
  const dx = b.x - a.x
  const dy = b.y - a.y
  const len = Math.hypot(dx, dy) || 1
  const cx = (a.x + b.x) / 2 - (dy / len) * bow
  const cy = (a.y + b.y) / 2 + (dx / len) * bow
  const u = 1 - t
  return {
    x: u * u * a.x + 2 * u * t * cx + t * t * b.x,
    y: u * u * a.y + 2 * u * t * cy + t * t * b.y,
  }
}

// Steps become a phase list. A step marked motion:'travel' is itself the travel: the
// dot glides to the step's anchor wearing the step's label. Any other anchor
// change gets a plain connector hop before the hold. Settle dwells are a
// traced-flow luxury, so the live flow skips them.
function phasesOf(trace: Trace, flow: FlowMode): Phase[] {
  const phases: Phase[] = []
  let prevAnchor: string | null = null
  trace.steps.forEach((step, i) => {
    const anchorId = anchorIdFor(step.at, trace)
    const moved = prevAnchor !== null && anchorId !== prevAnchor
    if (step.motion === 'travel' && prevAnchor && moved) {
      phases.push({
        kind: 'travel',
        fromId: prevAnchor,
        toId: anchorId,
        label: step.short,
        stepIndex: i,
        zoom: step.pace === 'long',
      })
      // a zoom leg is over in a blink, so the step's text earns a real hold
      // at the destination before the next step takes the panel
      if (step.pace === 'long') phases.push({ kind: 'settle', anchorId, hold: true })
    } else {
      if (prevAnchor && moved) {
        phases.push({
          kind: 'travel',
          fromId: prevAnchor,
          toId: anchorId,
          label: `${trace.event.fromService || trace.event.viaNode}→${trace.event.toService}`,
        })
      }
      phases.push({
        kind: 'pause',
        anchorId,
        stepIndex: i,
        short: step.short,
        quick: step.pace === 'short',
      })
    }
    prevAnchor = anchorId
  })
  if (flow === 'live') return phases
  const paced: Phase[] = []
  phases.forEach((ph, i) => {
    paced.push(ph)
    const next = phases[i + 1]
    if (ph.kind === 'travel' && (!next || next.kind === 'travel')) {
      paced.push({ kind: 'settle', anchorId: ph.toId })
    }
  })
  return paced
}

export function TrafficDotLayer({
  layoutRef,
  disabled,
  flow,
}: {
  layoutRef: React.RefObject<Layout | null>
  disabled: boolean
  flow: FlowMode
}) {
  const client = useClient()
  const svgRef = useRef<SVGSVGElement>(null)

  useEffect(() => {
    if (disabled) return
    const svg = svgRef.current
    if (!svg) return

    const pace = PACE[flow]
    const flights: Flight[] = []
    let raf = 0
    // traced flow alternates between a local story and an internet one: an
    // event repeating the last locality waits a few beats for its opposite,
    // since some pairs only exist in one locality (a singleton service is
    // always remote to other nodes)
    let lastRemote: boolean | null = null
    let skipsLeft = 3

    const makeDot = (color: string): SVGCircleElement => {
      const dot = document.createElementNS(SVGNS, 'circle')
      dot.setAttribute('r', '6.5')
      dot.setAttribute('cx', '-30')
      dot.setAttribute('cy', '-30')
      dot.setAttribute('fill', color)
      dot.setAttribute('class', 'traffic-dot')
      svg.appendChild(dot)
      return dot
    }
    const makeLabel = (color: string, size: number): SVGTextElement => {
      const label = document.createElementNS(SVGNS, 'text')
      label.setAttribute('fill', color)
      label.setAttribute('font-size', String(size))
      label.setAttribute('font-weight', '700')
      label.setAttribute('text-anchor', 'middle')
      label.setAttribute('stroke', 'var(--card)')
      label.setAttribute('stroke-width', '4')
      label.setAttribute('paint-order', 'stroke')
      label.style.fontFamily = 'var(--font-hand)'
      svg.appendChild(label)
      return label
    }

    const denyFlare = (at: Point) => {
      const g = document.createElementNS(SVGNS, 'g')
      g.setAttribute('stroke', 'var(--destructive)')
      g.setAttribute('stroke-width', '4')
      g.setAttribute('stroke-linecap', 'round')
      const s = 11
      for (const [x1, y1, x2, y2] of [
        [at.x - s, at.y - s, at.x + s, at.y + s],
        [at.x + s, at.y - s, at.x - s, at.y + s],
      ]) {
        const line = document.createElementNS(SVGNS, 'line')
        line.setAttribute('x1', String(x1))
        line.setAttribute('y1', String(y1))
        line.setAttribute('x2', String(x2))
        line.setAttribute('y2', String(y2))
        g.appendChild(line)
      }
      g.style.transition = 'opacity 0.6s ease'
      svg.appendChild(g)
      requestAnimationFrame(() => {
        g.style.opacity = '0'
      })
      setTimeout(() => g.remove(), 650)
    }

    const landingPulse = (instance: string) => {
      const el = document.querySelector(`[data-testid="instance-${instance}"]`)
      if (!el) return
      el.classList.remove('landing-pulse')
      void (el as HTMLElement).offsetWidth
      el.classList.add('landing-pulse')
    }

    const endFlight = (f: Flight) => {
      if (f.denyAtEnd) {
        const at = layoutRef.current?.anchors[anchorIdFor('rbac', f.trace)]
        if (at) denyFlare(at)
      }
      f.dot.remove()
      f.route.remove()
      f.step?.remove()
      if (flow === 'traced') traceStore.finish()
      const i = flights.indexOf(f)
      if (i >= 0) flights.splice(i, 1)
    }

    const stepFlight = (f: Flight, now: number, anchors: Record<string, Point>) => {
      const phase = f.phases[f.phase]
      let dur =
        phase.kind === 'settle'
          ? phase.hold
            ? pace.pause * 0.9
            : pace.settle
          : phase.kind === 'pause' && phase.quick
            ? pace.pause * 0.55
            : pace.pause
      if (phase.kind === 'travel') {
        if (phase.stepIndex !== undefined) dur = phase.zoom ? pace.stepTravel * 0.75 : pace.stepTravel
        else {
          const a = anchors[phase.fromId]
          const b = anchors[phase.toId]
          const dist = a && b ? Math.hypot(b.x - a.x, b.y - a.y) : Infinity
          dur = dist < 140 ? pace.shortHop : pace.travel
        }
      }
      const k = dur <= 0 ? 1 : Math.min(1, (now - f.phaseStart) / dur)

      let p: Point | undefined
      if (phase.kind !== 'travel') {
        p = anchors[phase.anchorId]
        if (p) p = { x: p.x, y: p.y - 2 }
      } else {
        const from = anchors[phase.fromId]
        const to = anchors[phase.toId]
        if (from && to) p = arc(from, to, easeTravel(k), bowFor(from, to))
      }
      if (!p) {
        // an endpoint of this trace left the board, so let it go quietly
        endFlight(f)
        return
      }
      f.dot.setAttribute('cx', String(p.x))
      f.dot.setAttribute('cy', String(p.y))
      f.route.setAttribute('x', String(p.x))
      f.route.setAttribute('y', String(p.y - 34))
      if (f.step) {
        f.step.setAttribute('x', String(p.x))
        f.step.setAttribute('y', String(p.y - 16))
      }

      if (k >= 1) {
        if (f.phase >= f.phases.length - 1) {
          endFlight(f)
          return
        }
        if (phase.kind === 'travel' && f.pulseTarget && phase.toId === `instance:${f.pulseTarget}`) {
          landingPulse(f.pulseTarget)
        }
        f.phase++
        f.phaseStart = now
        const next = f.phases[f.phase]
        if (!f.step) return // live flow keeps the route label and nothing else
        // connector hops and settles keep the previous step's text
        if (next.kind === 'pause') {
          f.step.textContent = `${next.stepIndex + 1}. ${next.short}`
          traceStore.step(next.stepIndex)
        } else if (next.kind === 'travel' && next.stepIndex !== undefined) {
          f.step.textContent = `${next.stepIndex + 1}. ${next.label}`
          traceStore.step(next.stepIndex)
        }
      }
    }

    const frame = (now: number) => {
      if (flights.length === 0) {
        raf = 0
        return
      }
      const anchors = layoutRef.current?.anchors ?? {}
      // iterate a copy: endFlight splices the source array
      for (const f of [...flights]) stepFlight(f, now, anchors)
      raf = flights.length > 0 ? requestAnimationFrame(frame) : 0
    }

    const spawn = (e: Parameters<Parameters<typeof client.watchTraffic>[0]>[0]) => {
      // Traced flies one story at a time, live flies everything up to the
      // cap, and the rail records every call either way.
      if (flow === 'traced' && flights.length > 0) return
      if (flights.length >= LIVE_FLIGHT_CAP) return
      const trace = buildTrace(e, clusterStore.getSnapshot())
      if (flow === 'traced' && e.verdict === 'allowed' && trace.targetNode) {
        const remote = trace.targetNode !== e.viaNode
        if (remote === lastRemote && skipsLeft > 0) {
          skipsLeft--
          return
        }
        lastRemote = remote
        skipsLeft = 3
      }
      const anchors = layoutRef.current?.anchors ?? {}
      // wait for layout: a flight needs at least its starting anchor
      if (!anchors[anchorIdFor(trace.steps[0].at, trace)]) return
      const color =
        e.verdict === 'allowed'
          ? 'var(--success)'
          : e.reason === 'no-endpoints'
            ? 'var(--ghost)'
            : 'var(--destructive)'
      const phases = phasesOf(trace, flow)
      const dot = makeDot(color)
      const route = makeLabel(color, flow === 'live' ? 13.5 : 17)
      route.textContent = `${e.fromService || e.viaNode}→${e.toService}`
      let step: SVGTextElement | null = null
      if (flow === 'traced') {
        step = makeLabel(color, 13.5)
        step.textContent = `1. ${trace.steps[0].short}`
        traceStore.set({ trace, stepIndex: 0, done: false })
      }
      flights.push({
        trace,
        phases,
        phase: 0,
        phaseStart: performance.now(),
        color,
        denyAtEnd: e.verdict === 'denied' && e.reason === 'policy',
        pulseTarget: e.verdict === 'allowed' ? e.toInstance : undefined,
        dot,
        route,
        step,
      })
      if (!raf) raf = requestAnimationFrame(frame)
    }

    const unsubscribe = client.watchTraffic(spawn)
    return () => {
      unsubscribe()
      if (raf) cancelAnimationFrame(raf)
      for (const f of flights) {
        f.dot.remove()
        f.route.remove()
        f.step?.remove()
      }
      if (flow === 'traced' && flights.length > 0) {
        traceStore.set(null) // a mid-flight unmount must not leave the store busy
      }
      flights.length = 0
    }
  }, [client, disabled, layoutRef, flow])

  if (disabled) return null
  return <svg ref={svgRef} className="pointer-events-none absolute inset-0 size-full" aria-hidden />
}
