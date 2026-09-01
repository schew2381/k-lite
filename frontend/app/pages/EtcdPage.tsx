// The etcd browser shows every key the cluster's memory holds, live. Rows flash as
// their mod revision moves, groups follow the store's key prefixes, and any
// row expands into the full object as YAML.

import { useEffect, useRef, useState } from 'react'
import { stringify } from 'yaml'
import type { Kind, KliteObject } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { useSnapshot } from '@/store/store'

type ObjectMap = 'nodes' | 'workloads' | 'services' | 'policies' | 'instances' | 'vipAllocations'

const GROUPS: { kind: Kind; prefix: string; map: ObjectMap }[] = [
  { kind: 'Node', prefix: '/klite/v1/nodes', map: 'nodes' },
  { kind: 'Workload', prefix: '/klite/v1/workloads', map: 'workloads' },
  { kind: 'Service', prefix: '/klite/v1/services', map: 'services' },
  { kind: 'NetworkPolicy', prefix: '/klite/v1/networkpolicies', map: 'policies' },
  { kind: 'Instance', prefix: '/klite/v1/instances', map: 'instances' },
  { kind: 'VIPAllocation', prefix: '/klite/v1/vipallocations', map: 'vipAllocations' },
]

function summarize(obj: KliteObject): string {
  switch (obj.kind) {
    case 'Workload':
      return `replicas ${obj.spec.replicas} · ${obj.spec.template.containers[0].image}`
    case 'Service':
      return `:${obj.spec.port}→${obj.spec.targetPort} · selects ${Object.entries(obj.spec.selector)
        .map(([k, v]) => `${k}=${v}`)
        .join(',')}`
    case 'Node':
      return `${obj.status?.phase ?? 'NotReady'}${obj.status?.unschedulable ? ' · cordoned' : ''} · ${obj.status?.instanceCount ?? 0} instances`
    case 'NetworkPolicy':
      return `${obj.spec.action} · ${obj.spec.rules.map((r) => `${r.from}→${r.to}`).join(', ')}`
    case 'Instance':
      return `${obj.status.phase} · ${obj.spec.node ?? 'unbound'} · ${obj.status.instanceIp ?? 'no ip'}`
    case 'VIPAllocation':
      return `${obj.spec.vip} · ${obj.spec.service} on ${obj.spec.node}`
  }
}

function Row({ etcdKey, obj }: { etcdKey: string; obj: KliteObject }) {
  const rv = obj.metadata.resourceVersion ?? 0
  const prevRv = useRef(rv)
  const [flash, setFlash] = useState(false)

  useEffect(() => {
    if (prevRv.current !== rv) {
      prevRv.current = rv
      setFlash(true)
      const t = setTimeout(() => setFlash(false), 900)
      return () => clearTimeout(t)
    }
  }, [rv])

  return (
    <details
      className={cn('group rounded-md transition-colors', flash && 'bg-accent')}
      data-testid="etcd-row"
    >
      <summary className="flex cursor-pointer items-baseline gap-3 rounded-md px-2 py-1.5 font-mono text-[12.5px] hover:bg-muted [&::-webkit-details-marker]:hidden">
        <span className="min-w-0 flex-1 truncate font-semibold">{etcdKey}</span>
        <span className="hidden text-muted-foreground sm:block">{summarize(obj)}</span>
        <Badge variant="outline" className="shrink-0 font-mono text-[9px] tabular-nums">
          rev {rv}
        </Badge>
      </summary>
      <pre className="mx-2 mb-2 overflow-x-auto rounded-lg border border-board-edge bg-card p-3 text-[11.5px] leading-4.5">
        {stringify(obj)}
      </pre>
    </details>
  )
}

export default function EtcdPage() {
  const snapshot = useSnapshot()
  const total = GROUPS.reduce((n, g) => n + Object.keys(snapshot[g.map]).length, 0)

  return (
    <div className="flex flex-col gap-4">
      <div className="boardbox flex flex-wrap items-baseline gap-x-6 gap-y-1 px-4 py-3">
        <span className="font-mono text-sm font-bold">etcd — the cluster's memory</span>
        <span className="font-mono text-xs text-muted-foreground" data-testid="etcd-rev">
          rev {snapshot.rev}
        </span>
        <span className="font-mono text-xs text-muted-foreground">{total} keys</span>
        <span className="flex items-center gap-1.5 font-mono text-[10px] text-traffic">
          <span className="size-2 animate-pulse rounded-full bg-traffic" /> live
        </span>
        <span className="hand basis-full text-xs text-muted-foreground">
          every apply, bind, heartbeat, and drain lands here first. Rows flash as their revision moves
        </span>
      </div>

      {GROUPS.map((group) => {
        const entries = Object.values<KliteObject>(snapshot[group.map]).sort((a, b) =>
          a.metadata.name.localeCompare(b.metadata.name),
        )
        return (
          <section key={group.kind} className="boardbox p-3">
            <div className="mb-1 flex items-baseline justify-between px-2">
              <h2 className="eyebrow">{group.prefix}/</h2>
              <span className="font-mono text-[10px] text-muted-foreground">
                {entries.length} key{entries.length === 1 ? '' : 's'}
              </span>
            </div>
            {entries.length === 0 ? (
              <p className="px-2 pb-1 font-mono text-xs text-muted-foreground">(empty prefix)</p>
            ) : (
              entries.map((obj) => (
                <Row
                  key={obj.metadata.name}
                  etcdKey={`${group.prefix}/${obj.metadata.name}`}
                  obj={obj}
                />
              ))
            )}
          </section>
        )
      })}
    </div>
  )
}
