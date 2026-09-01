// The dot layer plays one traced request at a time. The dot carries a
// two-line label: the route (a→b) stays on top for the whole flight, and a
// numbered step line below it changes only when a step starts. Most steps
// hold the dot at an anchor, and steps marked motion:'travel' are told while
// the dot glides to the next one. The trace panel prints the full sentences
// in sync. The implementation is one SVG and one requestAnimationFrame loop,
// with no React render per frame. Anchors come from the layout ref each
// frame, so a mid-flight layout shift bends the path instead of breaking it.

import { useEffect, useRef } from 'react'
import type { Layout, Point } from '@/layout/layout'
import { useClient } from '@/lib/client-context'
import { clusterStore } from '@/store/store'
import { traceStore } from '@/store/traceStore'
import { buildTrace, type Trace, type TraceStep } from '@/topo/trace'

const PAUSE_MS = 4000 // the dot holds at each step long enough to read it
const STEP_TRAVEL_MS = 2200 // a step told in motion still has to be readable
const SETTLE_MS = 1400 // arrival dwell: the dot rests where it landed before moving on
const TRAVEL_MS = 1200 // connector hop between steps that both pause
const SHORT_HOP_MS = 500 // hop between adjacent sub-boxes inside one infra pod
const SVGNS = 'http://www.w3.org/2000/svg'

type Phase =
  | { kind: 'pause'; anchorId: string; stepIndex: number; short: string }
  | {
      kind: 'travel'
      fromId: string
      toId: string
      label: string
      stepIndex?: number
      straight?: boolean
    }
  | { kind: 'settle'; anchorId: string } // rest after landing, label unchanged

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
  step: SVGTextElement // "3. dial 10.44.64.7:8080", swaps only at step starts
}

// trapezoidal velocity: ramp to full speed over the first 15% of the flight,
// cruise, brake over the last 15% — quick off the mark, no floaty landing
const RAMP = 0.15
function easeTravel(k: number): number {
  const s = 1 / (1 - RAMP)
  if (k < RAMP) return ((k * k) / (2 * RAMP)) * s
  if (k <= 1 - RAMP) return (RAMP / 2 + (k - RAMP)) * s
  return (1 - RAMP - ((1 - k) * (1 - k)) / (2 * RAMP)) * s
}

// which infra pod a sub-box anchor belongs to, or null for instance anchors
function podOf(anchorId: string): string | null {
  const m = /^(?:kdns|lds|rbac|eds):(.*)$/.exec(anchorId)
  return m ? m[1] : null
}

// bow 0 collapses the curve to a straight segment
function bezier(a: Point, b: Point, t: number, bow: number): Point {
  const mx = (a.x + b.x) / 2
  const my = (a.y + b.y) / 2
  const dx = b.x - a.x
  const dy = b.y - a.y
  const len = Math.hypot(dx, dy) || 1
  const cx = mx - (dy / len) * bow
  const cy = my + (dx / len) * bow
  const u = 1 - t
  return {
    x: u * u * a.x + 2 * u * t * cx + t * t * b.x,
    y: u * u * a.y + 2 * u * t * cy + t * t * b.y,
  }
}

function anchorIdFor(at: TraceStep['at'], trace: Trace): string {
  const e = trace.event
  if (at === 'caller') return `instance:${e.fromInstance}`
  if (at === 'target') return `instance:${e.toInstance}`
  return `${at}:${e.viaNode}` // kdns | lds | rbac | eds sub-box on the caller's node
}

// Steps become a phase list. A step marked motion:'travel' is itself the travel: the
// dot glides to the step's anchor wearing the step's label. Any other anchor
// change gets a plain connector hop before the hold.
function phasesOf(trace: Trace): Phase[] {
  const phases: Phase[] = []
  let prevAnchor: string | null = null
  trace.steps.forEach((step, i) => {
    const anchorId = anchorIdFor(step.at, trace)
    const moved = prevAnchor !== null && anchorId !== prevAnchor
    // hops between sub-boxes of one infra pod run straight, not arced
    const straight =
      prevAnchor !== null && podOf(prevAnchor) !== null && podOf(prevAnchor) === podOf(anchorId)
    if (step.motion === 'travel' && prevAnchor && moved) {
      phases.push({
        kind: 'travel',
        fromId: prevAnchor,
        toId: anchorId,
        label: step.short,
        stepIndex: i,
        straight,
      })
    } else {
      if (prevAnchor && moved) {
        phases.push({
          kind: 'travel',
          fromId: prevAnchor,
          toId: anchorId,
          label: `${trace.event.fromService}→${trace.event.toService}`,
          straight,
        })
      }
      phases.push({ kind: 'pause', anchorId, stepIndex: i, short: step.short })
    }
    prevAnchor = anchorId
  })
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
}: {
  layoutRef: React.RefObject<Layout | null>
  disabled: boolean
}) {
  const client = useClient()
  const svgRef = useRef<SVGSVGElement>(null)

  useEffect(() => {
    if (disabled) return
    const svg = svgRef.current
    if (!svg) return

    let flight: Flight | null = null
    let raf = 0

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
      f.step.remove()
      traceStore.finish()
      flight = null
    }

    const frame = (now: number) => {
      const f = flight
      if (!f) {
        raf = 0
        return
      }
      const anchors = layoutRef.current?.anchors ?? {}
      const phase = f.phases[f.phase]
      let dur = phase.kind === 'settle' ? SETTLE_MS : PAUSE_MS
      if (phase.kind === 'travel') {
        if (phase.stepIndex !== undefined) dur = STEP_TRAVEL_MS
        else {
          const a = anchors[phase.fromId]
          const b = anchors[phase.toId]
          const dist = a && b ? Math.hypot(b.x - a.x, b.y - a.y) : Infinity
          dur = dist < 140 ? SHORT_HOP_MS : TRAVEL_MS
        }
      }
      const k = Math.min(1, (now - f.phaseStart) / dur)

      let p: Point | undefined
      if (phase.kind !== 'travel') {
        p = anchors[phase.anchorId]
        if (p) p = { x: p.x, y: p.y - 2 }
      } else {
        const from = anchors[phase.fromId]
        const to = anchors[phase.toId]
        if (from && to) p = bezier(from, to, easeTravel(k), phase.straight ? 0 : 24)
      }
      if (!p) {
        // an endpoint of this trace left the board, so let it go quietly
        endFlight(f)
        raf = 0
        return
      }
      f.dot.setAttribute('cx', String(p.x))
      f.dot.setAttribute('cy', String(p.y))
      f.route.setAttribute('x', String(p.x))
      f.route.setAttribute('y', String(p.y - 34))
      f.step.setAttribute('x', String(p.x))
      f.step.setAttribute('y', String(p.y - 16))

      if (k >= 1) {
        if (f.phase >= f.phases.length - 1) {
          endFlight(f)
          raf = 0
          return
        }
        if (phase.kind === 'travel' && f.pulseTarget && phase.toId === `instance:${f.pulseTarget}`) {
          landingPulse(f.pulseTarget)
        }
        f.phase++
        f.phaseStart = now
        const next = f.phases[f.phase]
        // connector hops and settles keep the previous step's text
        if (next.kind === 'pause') {
          f.step.textContent = `${next.stepIndex + 1}. ${next.short}`
          traceStore.step(next.stepIndex)
        } else if (next.kind === 'travel' && next.stepIndex !== undefined) {
          f.step.textContent = `${next.stepIndex + 1}. ${next.label}`
          traceStore.step(next.stepIndex)
        }
      }
      raf = requestAnimationFrame(frame)
    }

    const spawn = (e: Parameters<Parameters<typeof client.watchTraffic>[0]>[0]) => {
      // one story at a time. The rail still records every call
      if (flight) return
      const trace = buildTrace(e, clusterStore.getSnapshot())
      const anchors = layoutRef.current?.anchors ?? {}
      if (!anchors[anchorIdFor('caller', trace)] || !anchors[anchorIdFor('kdns', trace)]) return
      const color =
        e.verdict === 'allowed'
          ? 'var(--success)'
          : e.reason === 'no-endpoints'
            ? 'var(--ghost)'
            : 'var(--destructive)'
      const phases = phasesOf(trace)
      const dot = makeDot(color)
      const route = makeLabel(color, 17)
      route.textContent = `${e.fromService}→${e.toService}`
      const step = makeLabel(color, 13.5)
      step.textContent = `1. ${trace.steps[0].short}`
      flight = {
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
      }
      traceStore.set({ trace, stepIndex: 0, done: false })
      if (!raf) raf = requestAnimationFrame(frame)
    }

    const unsubscribe = client.watchTraffic(spawn)
    return () => {
      unsubscribe()
      if (raf) cancelAnimationFrame(raf)
      if (flight) {
        flight.dot.remove()
        flight.route.remove()
        flight.step.remove()
        traceStore.set(null) // a mid-flight unmount must not leave the store busy
      }
    }
  }, [client, disabled, layoutRef])

  if (disabled) return null
  return <svg ref={svgRef} className="pointer-events-none absolute inset-0 size-full" aria-hidden />
}
