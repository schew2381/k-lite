import { MinusIcon, PlusIcon, Trash2Icon } from 'lucide-react'
import { toast } from 'sonner'
import { endpointStateOf, type Kind } from '@/api/types'
import { InstancePhaseBadge, NodePhaseBadge } from '@/components/PhaseBadge'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import {
  endpointsOf,
  instancesByNode,
  sortedInstances,
  sortedNodes,
  sortedPolicies,
  sortedServices,
  workloadRows,
} from '@/store/selectors'
import { useSnapshot } from '@/store/store'

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="boardbox p-4">
      <h2 className="eyebrow mb-3">{title}</h2>
      {children}
    </section>
  )
}

function NoRows({ title, hint }: { title: string; hint: string }) {
  return (
    <Empty className="postit-surface mx-auto my-2 max-w-sm p-4">
      <EmptyHeader>
        <EmptyTitle className="hand text-base">{title}</EmptyTitle>
        <EmptyDescription>{hint}</EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}

export default function ResourcesPage() {
  const client = useClient()
  const snapshot = useSnapshot()
  const rows = workloadRows(snapshot)
  // derived, like the board's node cards — status.instanceCount lags a sweep
  const byNode = instancesByNode(snapshot)

  const remove = (kind: Kind, name: string) =>
    act(
      client.remove(kind, name).then(() => {
        toast(`${kind.toLowerCase()} ${name} deleted`)
      }),
    )

  return (
    <div className="flex flex-col gap-5">
      <Section title="workloads">
        {rows.length === 0 ? (
          <NoRows
            title="no workloads yet"
            hint="apply a Workload from the apply page to run something"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>name</TableHead>
                <TableHead>image</TableHead>
                <TableHead>replicas</TableHead>
                <TableHead>ready</TableHead>
                <TableHead>service</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map(({ workload, ready, total, service }) => (
                <TableRow key={workload.metadata.name} data-testid={`wl-${workload.metadata.name}`}>
                  <TableCell className="font-mono font-semibold">{workload.metadata.name}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {workload.spec.template.containers[0].image}
                  </TableCell>
                  <TableCell>
                    <span className="flex items-center gap-1.5">
                      <Button
                        variant="outline"
                        size="icon-sm"
                        aria-label={`Scale ${workload.metadata.name} down`}
                        disabled={workload.spec.replicas === 0}
                        onClick={() =>
                          act(client.scale(workload.metadata.name, workload.spec.replicas - 1))
                        }
                      >
                        <MinusIcon />
                      </Button>
                      <span className="w-5 text-center font-mono">{workload.spec.replicas}</span>
                      <Button
                        variant="outline"
                        size="icon-sm"
                        aria-label={`Scale ${workload.metadata.name} up`}
                        onClick={() =>
                          act(client.scale(workload.metadata.name, workload.spec.replicas + 1))
                        }
                      >
                        <PlusIcon />
                      </Button>
                    </span>
                  </TableCell>
                  <TableCell className="font-mono">
                    {ready}/{total}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{service?.metadata.name ?? '—'}</TableCell>
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Delete ${workload.metadata.name}`}
                      onClick={() => remove('Workload', workload.metadata.name)}
                    >
                      <Trash2Icon className="text-deny" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>

      <Section title="instances">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>name</TableHead>
              <TableHead>workload</TableHead>
              <TableHead>node</TableHead>
              <TableHead>phase</TableHead>
              <TableHead>endpoint</TableHead>
              <TableHead>restarts</TableHead>
              <TableHead>ip</TableHead>
              <TableHead className="w-12" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {sortedInstances(snapshot).map((inst) => (
              <TableRow key={inst.metadata.name}>
                <TableCell className="font-mono font-semibold">{inst.metadata.name}</TableCell>
                <TableCell className="font-mono text-xs">{inst.spec.workload}</TableCell>
                <TableCell className="font-mono text-xs">{inst.spec.node ?? '(pending)'}</TableCell>
                <TableCell>
                  <InstancePhaseBadge phase={inst.status.phase} />
                </TableCell>
                <TableCell className="font-mono text-xs">{endpointStateOf(inst) ?? '—'}</TableCell>
                <TableCell className="font-mono">
                  {inst.status.restarts > 0 ? `↻${inst.status.restarts}` : '0'}
                </TableCell>
                <TableCell className="font-mono text-xs">{inst.status.instanceIp ?? '—'}</TableCell>
                <TableCell>
                  {(inst.status.phase === 'Ready' || inst.status.phase === 'Running') && (
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Kill ${inst.metadata.name}`}
                      onClick={() => act(client.killInstance(inst.metadata.name))}
                    >
                      <Trash2Icon className="text-deny" />
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Section>

      <Section title="nodes">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>name</TableHead>
              <TableHead>phase</TableHead>
              <TableHead>instances</TableHead>
              <TableHead>infra pod</TableHead>
              <TableHead>actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sortedNodes(snapshot).map((node) => (
              <TableRow key={node.metadata.name}>
                <TableCell className="font-mono font-semibold">{node.metadata.name}</TableCell>
                <TableCell>
                  <NodePhaseBadge
                    phase={node.status?.phase ?? 'NotReady'}
                    cordoned={node.status?.unschedulable}
                  />
                </TableCell>
                <TableCell className="font-mono">
                  {(byNode.get(node.metadata.name) ?? []).length}/{node.spec.maxInstances}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {node.status?.infra?.ip ?? 'starting…'}
                </TableCell>
                <TableCell>
                  <span className="flex gap-1.5">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => act(client.cordon(node.metadata.name, !node.status?.unschedulable))}
                    >
                      {node.status?.unschedulable ? 'uncordon' : 'cordon'}
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => remove('Node', node.metadata.name)}
                    >
                      drain &amp; remove
                    </Button>
                  </span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Section>

      <Section title="services">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>name</TableHead>
              <TableHead>selector</TableHead>
              <TableHead>ports</TableHead>
              <TableHead>VIPs (one per node)</TableHead>
              <TableHead>endpoints</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sortedServices(snapshot).map((svc) => {
              const { ready, draining } = endpointsOf(snapshot, svc)
              return (
                <TableRow key={svc.metadata.name}>
                  <TableCell className="font-mono font-semibold">{svc.metadata.name}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {Object.entries(svc.spec.selector)
                      .map(([k, v]) => `${k}=${v}`)
                      .join(',')}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {svc.spec.port}→{svc.spec.targetPort}
                  </TableCell>
                  <TableCell>
                    <span className="flex flex-wrap gap-1">
                      {Object.values(snapshot.vipAllocations)
                        .filter((va) => va.spec.service === svc.metadata.name)
                        .map(({ spec: { node, vip } }) => (
                          <Badge key={node} variant="outline" className="font-mono text-[10px]">
                            {node}: {vip}
                          </Badge>
                        ))}
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    <span className="text-traffic">{ready.length} READY</span>
                    {draining.length > 0 && (
                      <span className="text-draining"> · {draining.length} DRAINING</span>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </Section>

      <Section title="network policies">
        {sortedPolicies(snapshot).length === 0 ? (
          <NoRows
            title="no policies yet"
            hint="Traffic defaults to allow. Apply one from the apply page."
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>name</TableHead>
                <TableHead>action</TableHead>
                <TableHead>rules</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {sortedPolicies(snapshot).map((p) => (
                <TableRow key={p.metadata.name} data-testid={`policy-${p.metadata.name}`}>
                  <TableCell className="font-mono font-semibold">{p.metadata.name}</TableCell>
                  <TableCell>
                    <Badge
                      variant="outline"
                      className={
                        p.spec.action === 'DENY'
                          ? 'border-deny font-mono text-[10px] text-deny'
                          : 'border-traffic font-mono text-[10px] text-traffic'
                      }
                    >
                      {p.spec.action}
                    </Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {p.spec.rules
                      .map(
                        (r) =>
                          `${r.from} → ${r.to}${r.except ? ` except [${r.except.join(', ')}]` : ''}`,
                      )
                      .join(' · ')}
                  </TableCell>
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Delete policy ${p.metadata.name}`}
                      onClick={() => remove('NetworkPolicy', p.metadata.name)}
                    >
                      <Trash2Icon className="text-deny" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>
    </div>
  )
}
