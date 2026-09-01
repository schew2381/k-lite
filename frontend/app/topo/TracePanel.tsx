// The trace panel has two jobs (ADR 0027). In traced flow it prints the
// active request's story step by step, in sync with the dot on the board, or
// on its own timer under reduced motion where there are no dots. In live
// flow it holds the latest call's full path without stepping, because live
// requests finish faster than anyone reads.

import { useEffect } from 'react'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import { clusterStore } from '@/store/store'
import { traceStore, useActiveTrace } from '@/store/traceStore'
import type { FlowMode } from '@/topo/flow'
import { buildTrace } from '@/topo/trace'

const REDUCED_STEP_MS = 3000
const LIVE_REFRESH_MS = 1200 // how often the live panel swaps to a newer call

// Reduced-motion drive: no dot layer runs, so traces advance on a timer here.
function useReducedMotionTraceFeed(enabled: boolean) {
  const client = useClient()
  useEffect(() => {
    if (!enabled) return
    let timer = 0
    const unsubscribe = client.watchTraffic((e) => {
      if (traceStore.busy()) return
      const trace = buildTrace(e, clusterStore.getSnapshot())
      traceStore.set({ trace, stepIndex: 0, done: false })
      let i = 0
      timer = window.setInterval(() => {
        i++
        if (i >= trace.steps.length) {
          window.clearInterval(timer)
          traceStore.finish()
        } else {
          traceStore.step(i)
        }
      }, REDUCED_STEP_MS)
    })
    return () => {
      unsubscribe()
      window.clearInterval(timer)
      traceStore.set(null) // a mid-trace unmount must not leave the store busy
    }
  }, [enabled, client])
}

// Live drive: the panel holds the latest call's whole story, refreshed at
// reading pace. The dot layer never touches the store in live flow, so this
// hook owns it.
function useLatestCallFeed(enabled: boolean) {
  const client = useClient()
  useEffect(() => {
    if (!enabled) return
    let lastShown = 0
    const unsubscribe = client.watchTraffic((e) => {
      const now = performance.now()
      if (now - lastShown < LIVE_REFRESH_MS) return
      lastShown = now
      const trace = buildTrace(e, clusterStore.getSnapshot())
      traceStore.set({ trace, stepIndex: trace.steps.length - 1, done: true })
    })
    return () => {
      unsubscribe()
      traceStore.set(null)
    }
  }, [enabled, client])
}

export function TracePanel({ reduced, flow }: { reduced: boolean; flow: FlowMode }) {
  useReducedMotionTraceFeed(flow === 'traced' && reduced)
  useLatestCallFeed(flow === 'live')
  const active = useActiveTrace()

  return (
    <div className="boardbox flex flex-col gap-2 p-3" data-testid="trace-panel">
      <div className="flex items-baseline justify-between">
        <span className="eyebrow">{flow === 'live' ? 'latest call' : 'request trace'}</span>
        {active && (
          <span className="font-mono text-[10px] text-muted-foreground">
            {active.trace.event.fromInstance} → {active.trace.event.toService}
          </span>
        )}
      </div>
      {!active ? (
        <p className="text-xs text-muted-foreground">
          {flow === 'live'
            ? 'Waiting for traffic. Calls fly the board at real speed, and this panel keeps the last path taken.'
            : 'Waiting for the next call. Each one walks the full path: resolve, the kdns round trip, the dial, RBAC, and the EDS pick.'}
        </p>
      ) : (
        <ol className="flex flex-col gap-1.5">
          {active.trace.steps.map((step, i) => {
            const state =
              i < active.stepIndex || active.done ? 'past' : i === active.stepIndex ? 'now' : 'next'
            return (
              <li
                // biome-ignore lint/suspicious/noArrayIndexKey: steps are fixed for the life of one trace
                key={`${active.trace.event.id}-${i}`}
                className={cn(
                  'flex gap-2 rounded-md px-2 py-1 text-xs leading-snug transition-colors',
                  state === 'now' && 'bg-accent',
                  state === 'next' && 'opacity-35',
                )}
                data-testid={state === 'now' ? 'trace-step-active' : undefined}
              >
                <span
                  className={cn(
                    'hand mt-0.5 min-w-4 text-center text-[13px] font-bold',
                    step.tone === 'deny'
                      ? 'text-deny'
                      : step.tone === 'allow'
                        ? 'text-traffic'
                        : 'text-ctrl',
                  )}
                >
                  {i + 1}
                </span>
                <span>{step.detail}</span>
              </li>
            )
          })}
        </ol>
      )}
    </div>
  )
}
