// The rail prints the last ten calls, newest first, each with its verdict
// and where it landed. Ten fit without scrolling on any board, which beats
// a scroll region that broke once tall boards pushed it off screen. Under
// prefers-reduced-motion it carries the whole story on its own.

import { cn } from '@/lib/utils'
import { useTrafficLog } from '@/store/trafficLog'

const RAIL_ROWS = 10

export function TrafficRail() {
  const events = useTrafficLog()
  const recent = events.slice(-RAIL_ROWS).reverse()

  return (
    <div className="boardbox flex flex-col p-3" data-testid="traffic-rail">
      <div className="eyebrow mb-2">live calls</div>
      <ul className="flex flex-col gap-1 font-mono text-[11px]">
        {recent.length === 0 && <li className="text-muted-foreground">waiting for traffic…</li>}
        {recent.map((e) => (
          <li key={e.id} className="flex items-baseline gap-1.5" data-testid={`rail-${e.verdict}`}>
            <span className={cn('font-bold', e.verdict === 'allowed' ? 'text-traffic' : 'text-deny')}>
              {e.verdict === 'allowed' ? '✓' : '✕'}
            </span>
            <span className="truncate">
              {e.fromInstance || e.viaNode} → {e.toService}
              {e.verdict === 'allowed' ? (
                <span className="text-muted-foreground">
                  {' '}
                  {e.toInstance}
                  {e.latencyMs !== undefined && ` · ${e.latencyMs}ms`}
                </span>
              ) : e.reason === 'no-endpoints' ? (
                <span className="text-muted-foreground"> no endpoints</span>
              ) : e.matchedRule ? (
                <span className="text-deny"> blocked by {e.matchedRule.policy}</span>
              ) : e.rbacPhase === 'deny' ? (
                <span className="text-deny"> blocked by a DENY policy</span>
              ) : (
                <span className="text-deny"> not on the allowlist</span>
              )}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}
