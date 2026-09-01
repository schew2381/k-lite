import { MinusIcon, PlusIcon, Trash2Icon } from 'lucide-react'
import { motion } from 'motion/react'
import { toast } from 'sonner'
import type { Service, Workload } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { Rect } from '@/layout/layout'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import { endpointsOf } from '@/store/selectors'
import type { Snapshot } from '@/store/store'

export function ServiceCard({
  service,
  workload,
  rect,
  snapshot,
  highlighted,
}: {
  service: Service
  workload?: Workload
  rect: Rect
  snapshot: Snapshot
  highlighted?: boolean
}) {
  const client = useClient()
  const { ready, draining } = endpointsOf(snapshot, service)
  const name = service.metadata.name

  // the mirror of the create dialog: the pair goes together, and the
  // instances drain out through the same reconcile everything else uses
  const removeService = () =>
    act(
      (workload ? client.remove('Workload', workload.metadata.name) : Promise.resolve())
        .then(() => client.remove('Service', name))
        .then(() => {
          toast(
            `service/${name} deleted${workload ? ` with workload/${workload.metadata.name}` : ''}. Instances drain out`,
          )
        }),
    )

  return (
    <motion.div
      layout
      initial={{ opacity: 0 }}
      animate={{ opacity: 1, x: rect.x, y: rect.y }}
      exit={{ opacity: 0 }}
      transition={{ type: 'spring', stiffness: 300, damping: 32 }}
      style={{ width: rect.w, height: rect.h, position: 'absolute', top: 0, left: 0 }}
      className={`rounded-lg border-[1.5px] border-ink bg-card px-3 py-2 shadow-[2px_3px_0_rgba(43,42,36,0.12)] ${
        highlighted ? 'ring-2 ring-ctrl ring-offset-2 ring-offset-background' : ''
      }`}
      data-testid={`service-${name}`}
    >
      <div className="group flex items-baseline justify-between">
        <span className="font-mono text-sm font-bold">{name}</span>
        <span className="flex items-center gap-1">
          <span className="font-mono text-[10px] text-muted-foreground">
            :{service.spec.port}→{service.spec.targetPort}
          </span>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                className="-mr-1 size-5 opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100"
                aria-label={`Delete service ${name}`}
                onClick={removeService}
                data-testid={`delete-service-${name}`}
              >
                <Trash2Icon className="text-deny" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>Delete {name} and its workload. Instances drain out.</TooltipContent>
          </Tooltip>
        </span>
      </div>
      <div className="mt-0.5 flex items-center gap-2 font-mono text-[10px]">
        <span className="text-traffic">{ready.length} READY</span>
        {draining.length > 0 && <span className="text-draining">{draining.length} DRAINING</span>}
        {ready.length === 0 && <span className="text-deny">no endpoints</span>}
      </div>
      {workload && (
        <div className="mt-1.5 flex items-center gap-1.5">
          <Button
            variant="outline"
            size="icon-sm"
            className="size-6"
            aria-label={`Run one fewer ${name} instance`}
            disabled={workload.spec.replicas === 0}
            onClick={() => act(client.scale(workload.metadata.name, workload.spec.replicas - 1))}
            data-testid={`scale-down-${name}`}
          >
            <MinusIcon />
          </Button>
          <span className="font-mono text-xs" data-testid={`replicas-${name}`}>
            ×{workload.spec.replicas}
          </span>
          <Button
            variant="outline"
            size="icon-sm"
            className="size-6"
            aria-label={`Run one more ${name} instance`}
            onClick={() => act(client.scale(workload.metadata.name, workload.spec.replicas + 1))}
            data-testid={`scale-up-${name}`}
          >
            <PlusIcon />
          </Button>
        </div>
      )}
    </motion.div>
  )
}
