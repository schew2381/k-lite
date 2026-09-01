// The trace panel prints the active request's story step by step, in sync
// with the dot on the board. Under reduced motion there are no dots, so the
// panel plays the steps on its own timer instead.

import { useEffect } from 'react'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import { clusterStore } from '@/store/store'
import { traceStore, useActiveTrace } from '@/store/traceStore'
import { buildTrace } from '@/topo/trace'

const REDUCED_STEP_MS = 3000

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

export function TracePanel({ reduced }: { reduced: boolean }) {
  useReducedMotionTraceFeed(reduced)
  const active = useActiveTrace()

  return (
    <div className="boardbox flex flex-col gap-2 p-3" data-testid="trace-panel">
      <div className="flex items-baseline justify-between">
        <span className="eyebrow">request trace</span>
        {active && (
          <span className="font-mono text-[10px] text-muted-foreground">
            {active.trace.event.fromInstance} → {active.trace.event.toService}
          </span>
        )}
      </div>
      {!active ? (
        <p className="text-xs text-muted-foreground">
          Waiting for the next call. Each one walks the full path, from the resolver through the kdns
          round trip and the dial to RBAC and the EDS pick.
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
