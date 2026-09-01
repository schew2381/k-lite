// Clicking a node's infra pod opens this sheet: kdns records, Envoy listeners,
// the compiled RBAC table, the EDS endpoint sets, and the identity map. Every
// row derives live from the store.

import type { NodeObj } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { cn } from '@/lib/utils'
import {
  dialTargetOf,
  endpointsOf,
  identityRows,
  infraIpOf,
  ingressRowsOf,
  rbacView,
  sortedServices,
  vipFor,
} from '@/store/selectors'
import { useSnapshot } from '@/store/store'

function Section({
  title,
  note,
  children,
}: {
  title: string
  note?: string
  children: React.ReactNode
}) {
  return (
    <section className="rounded-lg border-[1.5px] border-ink bg-card p-3">
      <div className="mb-2 flex items-baseline justify-between">
        <h3 className="font-mono text-xs font-bold uppercase tracking-wide">{title}</h3>
        {note && <span className="text-[10px] text-muted-foreground">{note}</span>}
      </div>
      {children}
    </section>
  )
}

const Row = ({ children, tone }: { children: React.ReactNode; tone?: 'deny' | 'allow' }) => (
  <div
    className={cn(
      'font-mono text-[12px] leading-6',
      tone === 'deny' && 'text-deny',
      tone === 'allow' && 'text-traffic',
    )}
  >
    {children}
  </div>
)

export function InfraPodSheet({
  node,
  open,
  onOpenChange,
}: {
  node: NodeObj
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const snapshot = useSnapshot()
  const name = node.metadata.name
  const services = sortedServices(snapshot)
  const rbac = rbacView(snapshot)
  const identities = identityRows(snapshot)

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-[420px] overflow-y-auto sm:max-w-[420px]" data-testid="infra-sheet">
        <SheetHeader>
          <SheetTitle className="font-mono">infra pod — {name}</SheetTitle>
          <SheetDescription>
            one shared netns · {infraIpOf(node) ?? 'starting…'} · klite-net owns it and Envoy joins it
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-col gap-3 px-4 pb-6">
          <Section title="klite-net · kdns" note="TTL 5s · answers even when policy denies">
            {services.map((svc) => (
              <Row key={svc.metadata.name}>
                {svc.metadata.name}.svc.klite <span className="text-muted-foreground">→</span>{' '}
                {vipFor(snapshot, svc.metadata.name, name) ?? '(no VIP yet)'}
              </Row>
            ))}
            {services.length === 0 && <Row>no services</Row>}
          </Section>

          <Section title="envoy · listeners (LDS)" note="one per VIP this node owns">
            {services.map((svc) => (
              <Row key={svc.metadata.name}>
                {vipFor(snapshot, svc.metadata.name, name) ?? '?'}:{svc.spec.port}{' '}
                <span className="text-muted-foreground">→ cluster</span> {svc.metadata.name}
              </Row>
            ))}
          </Section>

          <Section title="envoy · RBAC filter" note="DENY first, then ALLOW, then default">
            {rbac.deny.map((r, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: rules render in evaluation order and never reorder
              <Row key={`d${i}`} tone="deny">
                DENY {r.from} → {r.to}
                {r.except ? ` except [${r.except.join(', ')}]` : ''}{' '}
                <span className="text-muted-foreground">({r.policy})</span>
              </Row>
            ))}
            {rbac.allow.map((r, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: rules render in evaluation order and never reorder
              <Row key={`a${i}`} tone="allow">
                ALLOW {r.from} → {r.to}
                {r.except ? ` except [${r.except.join(', ')}]` : ''}{' '}
                <span className="text-muted-foreground">({r.policy})</span>
              </Row>
            ))}
            <Row>
              <span className="text-muted-foreground">
                → default:{' '}
                {rbac.allow.length === 0
                  ? 'allow (no ALLOW targets anything)'
                  : 'allow unless an ALLOW targets the service'}
              </span>
            </Row>
          </Section>

          <Section title="envoy · endpoints (EDS)" note="DRAINING = no new connections">
            {services.map((svc) => {
              const { ready, draining } = endpointsOf(snapshot, svc)
              return (
                <Row key={svc.metadata.name}>
                  {svc.metadata.name}:{' '}
                  {ready.length === 0 && draining.length === 0 && (
                    <span className="text-deny">no endpoints</span>
                  )}
                  {ready.map((i) => {
                    const dial = dialTargetOf(snapshot, svc, i, name)
                    return (
                      <span key={i.metadata.name}>
                        {i.metadata.name} <span className="text-traffic">READY</span>{' '}
                        {dial.local ? (
                          <span className="text-muted-foreground">local ({dial.address}) </span>
                        ) : (
                          <span className="text-ctrl">internet ({dial.address}) </span>
                        )}
                      </span>
                    )
                  })}
                  {draining.map((i) => (
                    <span key={i.metadata.name}>
                      {i.metadata.name} <span className="font-bold text-deny">DRAINING</span>{' '}
                    </span>
                  ))}
                </Row>
              )
            })}
          </Section>

          <Section title="envoy · ingress (mTLS)" note="what this node publishes for remote proxies">
            {node.status?.advertiseAddress ? (
              <Row>
                advertised as <span className="text-ctrl">{node.status.advertiseAddress}</span>
              </Row>
            ) : (
              <Row>not advertised yet</Row>
            )}
            {ingressRowsOf(snapshot, name).length === 0 ? (
              <Row>(nothing published yet)</Row>
            ) : (
              ingressRowsOf(snapshot, name).map((r) => (
                <Row key={r.port}>
                  :{r.port} <span className="text-muted-foreground">→</span> {r.instance}{' '}
                  <span className="text-muted-foreground">{r.forward}</span>
                </Row>
              ))
            )}
          </Section>

          <Section title="identity map" note="how RBAC knows who's calling">
            {identities.map((row) => (
              <Row key={row.ip}>
                {row.ip} <span className="text-muted-foreground">→</span> {row.instance}{' '}
                <Badge variant="outline" className="ml-1 font-mono text-[9px]">
                  {row.service}
                </Badge>
              </Row>
            ))}
            {identities.length === 0 && <Row>no running instances</Row>}
          </Section>
        </div>
      </SheetContent>
    </Sheet>
  )
}
