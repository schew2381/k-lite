import type { InstancePhase, NodePhase } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

// One palette for every phase chip, matching design.html's setInstState.
// Two renderings aren't plain colors: Pending is a ghost outline, and
// Terminating is half-erased. The map below is the whole story.
const INSTANCE_STYLES: Record<InstancePhase, string> = {
  Pending: 'border-dashed border-ghost text-muted-foreground bg-transparent',
  Running: 'border-ctrl text-ctrl bg-accent',
  Ready: 'border-traffic text-traffic bg-traffic/10',
  Draining: 'border-draining text-draining bg-draining/10',
  Failed: 'border-deny border-dashed text-deny bg-deny/10',
  Terminating: 'border-ghost text-ghost bg-transparent',
}

const NODE_STYLES: Record<NodePhase, string> = {
  Ready: 'border-traffic text-traffic bg-traffic/10',
  NotReady: 'border-deny text-deny bg-deny/10',
  Draining: 'border-draining text-draining bg-draining/10',
}

export function InstancePhaseBadge({ phase, className }: { phase: InstancePhase; className?: string }) {
  return (
    <Badge variant="outline" className={cn('font-mono text-[10px]', INSTANCE_STYLES[phase], className)}>
      {phase}
    </Badge>
  )
}

export function NodePhaseBadge({ phase, cordoned }: { phase: NodePhase; cordoned?: boolean }) {
  return (
    <span className="flex items-center gap-1.5">
      <Badge variant="outline" className={cn('font-mono text-[10px]', NODE_STYLES[phase])}>
        {phase}
      </Badge>
      {cordoned && (
        <Badge variant="outline" className="border-draining font-mono text-[10px] text-draining">
          cordoned
        </Badge>
      )}
    </span>
  )
}
