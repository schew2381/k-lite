// The rail prints every call, its verdict, and which instance it landed on.
// Under prefers-reduced-motion it carries the whole story on its own.

import { ScrollArea } from '@/components/ui/scroll-area'
import { cn } from '@/lib/utils'
import { useTrafficLog } from '@/store/trafficLog'

export function TrafficRail() {
  const events = useTrafficLog()
  const recent = events.slice(-30).reverse()

  return (
    <div className="boardbox flex h-full flex-col p-3" data-testid="traffic-rail">
      <div className="eyebrow mb-2">live calls</div>
      <ScrollArea className="min-h-0 flex-1">
        <ul className="flex flex-col gap-1 pr-2 font-mono text-[11px]">
          {recent.length === 0 && <li className="text-muted-foreground">waiting for traffic…</li>}
          {recent.map((e) => (
            <li key={e.id} className="flex items-baseline gap-1.5" data-testid={`rail-${e.verdict}`}>
              <span className={cn('font-bold', e.verdict === 'allowed' ? 'text-traffic' : 'text-deny')}>
                {e.verdict === 'allowed' ? '✓' : '✕'}
              </span>
              <span className="truncate">
                {e.fromInstance} → {e.toService}
                {e.verdict === 'allowed' ? (
                  <span className="text-muted-foreground">
                    {' '}
                    {e.toInstance} · {e.latencyMs}ms
                  </span>
                ) : e.reason === 'no-endpoints' ? (
                  <span className="text-muted-foreground"> no endpoints</span>
                ) : e.matchedRule ? (
                  <span className="text-deny"> blocked by {e.matchedRule.policy}</span>
                ) : (
                  <span className="text-deny"> not on the allowlist</span>
                )}
              </span>
            </li>
          ))}
        </ul>
      </ScrollArea>
    </div>
  )
}
