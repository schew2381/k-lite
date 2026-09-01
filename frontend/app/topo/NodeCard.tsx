import { MoreVerticalIcon } from 'lucide-react'
import { motion } from 'motion/react'
import { useState } from 'react'
import { toast } from 'sonner'
import type { NodeObj } from '@/api/types'
import { NodePhaseBadge } from '@/components/PhaseBadge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import type { NodeLayout, Rect } from '@/layout/layout'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import { endpointsOf, infraIpOf, rbacView, sortedServices, vipFor } from '@/store/selectors'
import { useSnapshot } from '@/store/store'
import { InfraPodSheet } from '@/topo/InfraPodSheet'

// positions a layout rect inside the absolutely-positioned node card
const rectStyle = (r: Rect, card: Rect) => ({
  position: 'absolute' as const,
  left: r.x - card.x,
  top: r.y - card.y,
  width: r.w,
  height: r.h,
})

function SubBox({
  rect,
  within,
  title,
  note,
  deny,
  plain,
  children,
}: {
  rect: Rect
  within: Rect
  title: string
  note?: string
  deny?: boolean
  plain?: boolean // outline only: the envoy frame whose children are boxes themselves
  children?: React.ReactNode
}) {
  return (
    <div
      style={rectStyle(rect, within)}
      className={cn('rounded-md border border-ink/35 px-1.5 pt-0.5', !plain && 'bg-card/70')}
    >
      <div className={cn('font-mono text-[10px] font-semibold', deny ? 'text-deny' : 'text-ink/80')}>
        {title}
        {note && <span className="font-normal text-muted-foreground"> ({note})</span>}
      </div>
      {children && <div className="font-mono text-[10px] leading-[15px]">{children}</div>}
    </div>
  )
}

export function NodeCard({ node, layout, count }: { node: NodeObj; layout: NodeLayout; count: number }) {
  const client = useClient()
  const snapshot = useSnapshot()
  const [inspecting, setInspecting] = useState(false)
  const services = sortedServices(snapshot)
  const rbac = rbacView(snapshot)
  const ruleLine = (rules: { from: string; to: string }[]) =>
    rules.map((r) => `${r.from}→${r.to}`).join(', ')
  const name = node.metadata.name
  const phase = node.status?.phase ?? 'NotReady'

  const cordoned = node.status?.unschedulable ?? false
  const cordon = (on: boolean) =>
    act(
      client.cordon(name, on).then(() => {
        toast(`${name} ${on ? 'cordoned. Nothing new schedules here' : 'uncordoned'}`)
      }),
    )
  const drain = () =>
    act(
      client.drainNode(name).then(() => {
        toast(`${name} is draining. Instances surge elsewhere first`)
      }),
    )
  const drainAndRemove = () =>
    act(
      client.remove('Node', name).then(() => {
        toast(`${name} is draining and will leave the cluster`)
      }),
    )

  return (
    <motion.div
      layout
      initial={{ opacity: 0 }}
      animate={{ opacity: 1, x: layout.card.x, y: layout.card.y }}
      exit={{ opacity: 0 }}
      transition={{ type: 'spring', stiffness: 300, damping: 32 }}
      style={{ width: layout.card.w, height: layout.card.h, position: 'absolute', top: 0, left: 0 }}
      className={cn(
        'rounded-xl border-2 border-ink bg-card/60 shadow-[3px_4px_0_rgba(43,42,36,0.10)]',
        phase === 'NotReady' && 'border-dashed border-deny',
      )}
      data-testid={`node-${name}`}
    >
      <div className="flex items-center justify-between px-3.5 pt-2.5">
        <div className="flex items-baseline gap-2">
          <span className="text-[15px] font-bold">{name}</span>
          <span className="font-mono text-[10px] text-muted-foreground">
            {count}/{node.spec.maxInstances}
          </span>
        </div>
        <div className="flex items-center gap-1.5">
          <NodePhaseBadge phase={phase} cordoned={cordoned} />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${name}`}>
                <MoreVerticalIcon />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuGroup>
                {client.can.cordon &&
                  (cordoned ? (
                    <DropdownMenuItem onClick={() => cordon(false)}>Uncordon</DropdownMenuItem>
                  ) : (
                    <DropdownMenuItem onClick={() => cordon(true)}>Cordon</DropdownMenuItem>
                  ))}
                {client.can.drain && <DropdownMenuItem onClick={drain}>Drain</DropdownMenuItem>}
                <DropdownMenuItem variant="destructive" onClick={drainAndRemove}>
                  Drain &amp; remove
                </DropdownMenuItem>
              </DropdownMenuGroup>
              {client.chaos && (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuGroup>
                    {phase === 'NotReady' ? (
                      <DropdownMenuItem onClick={() => client.chaos?.reviveNodeAgent(name)}>
                        Revive agent
                      </DropdownMenuItem>
                    ) : (
                      <DropdownMenuItem
                        onClick={() => {
                          client.chaos?.killNodeAgent(name)
                          toast(`${name}'s agent stopped heartbeating`)
                        }}
                      >
                        Kill agent
                      </DropdownMenuItem>
                    )}
                  </DropdownMenuGroup>
                </>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {/* The infra pod as sub-boxes: kdns, then envoy wrapping LDS, RBAC, and
          EDS. Rects come from the layout so traced dots land on the sub-box
          doing that step's work. Clicking anywhere opens the full inspector.
          The button's aria-label replaces the table text for screen readers,
          and the inspector sheet carries the same data in list form. */}
      <button
        type="button"
        className="absolute rounded-lg border-[1.5px] border-ctrl/70 bg-accent/40 text-left hover:border-ctrl focus-visible:outline-2 focus-visible:outline-ctrl"
        style={rectStyle(layout.infra.box, layout.card)}
        onClick={() => setInspecting(true)}
        aria-label={`Inspect infra pod on ${name}`}
        data-testid={`infra-${name}`}
      >
        <div className="absolute inset-x-0 top-0 flex items-baseline justify-between px-2 pt-1">
          <span className="font-mono text-[11px] font-semibold text-ctrl">
            infra pod{' '}
            <span className="font-normal text-muted-foreground">
              one shared netns · {infraIpOf(node) ?? 'starting…'}
            </span>
          </span>
          <span className="font-mono text-[10px] text-ctrl">inspect</span>
        </div>

        <SubBox rect={layout.infra.kdns} within={layout.infra.box} title="klite-net · kdns">
          {services.map((svc) => (
            <div key={svc.metadata.name} className="truncate">
              {svc.metadata.name}.svc.klite <span className="text-muted-foreground">→</span>{' '}
              {vipFor(snapshot, svc.metadata.name, name) ?? '…'}
            </div>
          ))}
          <div className="truncate text-[9.5px] text-muted-foreground">
            TTL 5s · owns the VIPs · probes instances
          </div>
        </SubBox>

        <SubBox
          rect={layout.infra.envoy}
          within={layout.infra.box}
          title="envoy"
          note="joined the netns"
          plain
        />
        <SubBox rect={layout.infra.lds} within={layout.infra.box} title="listeners (LDS)">
          {services.map((svc) => (
            <div key={svc.metadata.name} className="truncate">
              {vipFor(snapshot, svc.metadata.name, name) ?? '…'}:{svc.spec.port}{' '}
              <span className="text-muted-foreground">→</span> cluster {svc.metadata.name}
            </div>
          ))}
        </SubBox>
        <SubBox rect={layout.infra.rbac} within={layout.infra.box} title="RBAC filter" deny>
          <div className="truncate">
            {rbac.deny.length > 0 ? `DENY: ${ruleLine(rbac.deny)}` : 'DENY: (none)'}
          </div>
          <div className="truncate">
            {rbac.allow.length > 0
              ? `ALLOW: ${ruleLine(rbac.allow)} → allowlist on`
              : 'ALLOW: (none defined) → default: allow'}
          </div>
        </SubBox>
        <SubBox rect={layout.infra.eds} within={layout.infra.box} title="endpoints (EDS)">
          {services.map((svc) => {
            const eps = endpointsOf(snapshot, svc)
            const row = [
              ...eps.ready.map((i) => `${i.metadata.name} READY`),
              ...eps.draining.map((i) => `${i.metadata.name} DRAINING`),
            ]
            return (
              <div key={svc.metadata.name} className="truncate">
                {svc.metadata.name}: {row.length > 0 ? row.join(' · ') : '(none)'}
              </div>
            )
          })}
          <div className="truncate text-[9.5px] text-muted-foreground">
            DRAINING = no new connections
          </div>
        </SubBox>
      </button>
      {inspecting && <InfraPodSheet node={node} open onOpenChange={setInspecting} />}
    </motion.div>
  )
}
