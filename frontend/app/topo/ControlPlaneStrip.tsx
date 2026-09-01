// This strip sits above the board and shows who holds the lease, what etcd
// knows, and which streams are open. In mock mode the control plane is the
// in-browser simulator, and the strip says so.

import { StarIcon } from 'lucide-react'
import { Link } from 'react-router-dom'
import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import { sortedNodes } from '@/store/selectors'
import { useSnapshot } from '@/store/store'

function Card({
  title,
  corner,
  children,
}: {
  title: React.ReactNode
  corner?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <div className="min-w-56 flex-1 rounded-xl border-[1.5px] border-ink bg-card px-4 py-3 shadow-[2px_3px_0_rgba(43,42,36,0.12)]">
      <div className="mb-1.5 flex items-center justify-between gap-2">
        <span className="font-mono text-sm font-bold">{title}</span>
        {corner}
      </div>
      <div className="flex flex-col gap-1 text-xs leading-5 text-muted-foreground">{children}</div>
    </div>
  )
}

const Line = ({ label, children }: { label: string; children: React.ReactNode }) => (
  <div className="flex items-baseline gap-2">
    <span className="w-16 shrink-0 font-mono text-[10px] uppercase tracking-wide opacity-70">
      {label}
    </span>
    <span className="min-w-0 font-mono text-[11.5px] text-foreground">{children}</span>
  </div>
)

export function ControlPlaneStrip() {
  const client = useClient()
  const snapshot = useSnapshot()
  const nodes = sortedNodes(snapshot)
  const objectCount =
    Object.keys(snapshot.workloads).length +
    Object.keys(snapshot.services).length +
    Object.keys(snapshot.nodes).length +
    Object.keys(snapshot.policies).length +
    Object.keys(snapshot.instances).length

  return (
    <div className="mb-3 flex flex-wrap items-stretch gap-3" data-testid="control-plane">
      <Card
        title={
          <span className="flex items-center gap-1.5">
            klited ①
            <StarIcon className="size-4 fill-draining text-draining" aria-label="holds the lease" />
          </span>
        }
        corner={
          <Badge variant="outline" className="font-mono text-[9px]">
            gRPC · xDS · controllers
          </Badge>
        }
      >
        <Line label="lease">held: scheduler, rollout, endpoints, nodes awake</Line>
        <Line label="streams">
          <span className="flex flex-wrap items-center gap-1.5">
            {nodes.map((n) => (
              <Tooltip key={n.metadata.name}>
                <TooltipTrigger asChild>
                  <span
                    className={cn(
                      'rounded-full border px-2 py-px text-[10px]',
                      n.status?.phase === 'NotReady'
                        ? 'border-deny text-deny'
                        : 'border-traffic text-traffic',
                    )}
                  >
                    {n.metadata.name} {n.status?.phase === 'NotReady' ? '✕' : '⇅'}
                  </span>
                </TooltipTrigger>
                <TooltipContent>
                  {n.metadata.name}: WatchDesired + ReportStatus over mTLS gRPC
                </TooltipContent>
              </Tooltip>
            ))}
            <span className="rounded-full border border-ctrl px-2 py-px text-[10px] text-ctrl">
              this browser ⇣ watch
            </span>
          </span>
        </Line>
        <Line label="xDS">
          snapshots → {nodes.length} envoy{nodes.length === 1 ? '' : 's'} · traffic feed live
        </Line>
      </Card>

      <Card
        title="klited ②"
        corner={
          <Badge variant="outline" className="font-mono text-[9px]">
            gRPC · xDS · controllers
          </Badge>
        }
      >
        <Line label="lease">asleep: takes over if ① dies</Line>
        <Line label="serving">API and xDS answer here too</Line>
        {client.mode === 'mock' && (
          <span className="hand text-[11.5px]">
            both klited copies are simulated in this browser for now
          </span>
        )}
      </Card>

      <Card
        title="etcd × 3"
        corner={
          <Badge variant="outline" className="font-mono text-[9px]">
            kv
          </Badge>
        }
      >
        <Line label="rev">{snapshot.rev}</Line>
        <Line label="keys">{objectCount} objects</Line>
        <Line label="holds">
          all state · watches · the lease ·{' '}
          <Link to="/etcd" className="text-ctrl underline underline-offset-2">
            browse
          </Link>
        </Line>
      </Card>
    </div>
  )
}
