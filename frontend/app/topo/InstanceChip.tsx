import { XIcon } from 'lucide-react'
import { motion } from 'motion/react'
import type { Instance } from '@/api/types'
import { InstancePhaseBadge } from '@/components/PhaseBadge'
import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { Rect } from '@/layout/layout'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'

export function InstanceChip({ instance, rect }: { instance: Instance; rect: Rect }) {
  const client = useClient()
  const { phase, restarts, instanceIp } = instance.status
  const killable = phase === 'Running' || phase === 'Ready'

  return (
    <motion.div
      layout
      initial={{ opacity: 0, scale: 0.9 }}
      animate={{ opacity: 1, scale: 1, x: rect.x, y: rect.y }}
      exit={{ opacity: 0, scale: 0.95 }}
      transition={{ type: 'spring', stiffness: 350, damping: 30 }}
      style={{ width: rect.w, height: rect.h, position: 'absolute', top: 0, left: 0 }}
      className={cn(
        'group rounded-lg border-[1.5px] border-ink bg-card px-2 py-1.5 shadow-[2px_2px_0_rgba(43,42,36,0.12)]',
        phase === 'Terminating' && 'erased',
        phase === 'Pending' && 'border-dashed border-ghost shadow-none',
        phase === 'Failed' && 'border-dashed border-deny',
      )}
      data-testid={`instance-${instance.metadata.name}`}
      data-phase={phase}
    >
      <div className="flex items-start justify-between gap-1">
        <span className="truncate font-mono text-[13px] font-semibold">{instance.metadata.name}</span>
        {killable && (
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Kill ${instance.metadata.name}`}
                className="-mr-1 -mt-0.5 size-5 opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100"
                onClick={() => act(client.killInstance(instance.metadata.name))}
              >
                <XIcon className="text-deny" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>Kill this instance. The agent restarts it.</TooltipContent>
          </Tooltip>
        )}
      </div>
      <div className="mt-0.5 flex items-center gap-1.5">
        <InstancePhaseBadge phase={phase} />
        {restarts > 0 && (
          <span className="font-mono text-[10px] text-draining" title={`${restarts} restarts`}>
            ↻{restarts}
          </span>
        )}
      </div>
      <div className="mt-0.5 truncate font-mono text-[10px] text-muted-foreground">
        {instanceIp ?? '—'}
      </div>
    </motion.div>
  )
}
