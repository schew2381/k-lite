import { PlusIcon } from 'lucide-react'
import { AnimatePresence } from 'motion/react'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { computeLayout, type Layout } from '@/layout/layout'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import { newNodeYaml } from '@/lib/yamlgen'
import { instancesByNode, selectorMatches, sortedNodes, sortedServices } from '@/store/selectors'
import { useSnapshot } from '@/store/store'
import { AddServiceDialog } from '@/topo/AddServiceDialog'
import { ControlPlaneStrip } from '@/topo/ControlPlaneStrip'
import { InstanceChip } from '@/topo/InstanceChip'
import { NodeCard } from '@/topo/NodeCard'
import { PolicyArcs } from '@/topo/PolicyArcs'
import { PolicyBuilder } from '@/topo/PolicyBuilder'
import { PolicySimPanel } from '@/topo/PolicySimPanel'
import { ServiceCard } from '@/topo/ServiceStrip'
import { TracePanel } from '@/topo/TracePanel'
import { TrafficDotLayer } from '@/topo/TrafficDotLayer'
import { TrafficRail } from '@/topo/TrafficRail'

function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(
    () => window.matchMedia('(prefers-reduced-motion: reduce)').matches,
  )
  useEffect(() => {
    const mq = window.matchMedia('(prefers-reduced-motion: reduce)')
    const onChange = () => setReduced(mq.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])
  return reduced
}

// A callback ref, not a ref object: the board mounts after the sync skeleton
// in http mode, and an object ref would leave the observer unattached.
function useBoardWidth(): { ref: (el: HTMLDivElement | null) => void; width: number } {
  const [el, setEl] = useState<HTMLDivElement | null>(null)
  const [width, setWidth] = useState(1000)
  useEffect(() => {
    if (!el) return
    let timer = 0
    const observer = new ResizeObserver(([entry]) => {
      window.clearTimeout(timer)
      timer = window.setTimeout(() => setWidth(entry.contentRect.width), 100)
    })
    observer.observe(el)
    setWidth(el.clientWidth)
    return () => {
      observer.disconnect()
      window.clearTimeout(timer)
    }
  }, [el])
  return { ref: setEl, width }
}

export default function TopologyPage() {
  const client = useClient()
  const snapshot = useSnapshot()
  const reduced = useReducedMotion()
  const board = useBoardWidth()
  const [highlight, setHighlight] = useState<{ from: string; to: string } | null>(null)

  const layout = useMemo(() => computeLayout(snapshot, board.width), [snapshot, board.width])
  // dots read anchors per frame without re-rendering. Written post-commit so
  // an aborted render can't leave the rAF loop pointing at uncommitted math
  const layoutRef = useRef<Layout | null>(layout)
  useLayoutEffect(() => {
    layoutRef.current = layout
  }, [layout])

  const nodes = sortedNodes(snapshot)
  const services = sortedServices(snapshot)
  const byNode = instancesByNode(snapshot)

  const addNode = useCallback(() => {
    const taken = new Set(nodes.map((n) => n.metadata.name))
    let i = 1
    while (taken.has(`node-${i}`)) i++
    act(
      client.apply(newNodeYaml(`node-${i}`)).then(() => {
        toast(`node-${i} is joining. Its infra pod is starting`)
      }),
    )
  }, [client, nodes])

  if (!snapshot.synced) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  return (
    <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_300px]">
      <div>
        <div className="mb-3 flex items-center justify-between">
          <p className="text-sm text-muted-foreground">
            Instances call services by name. Each call rides through its node's infra pod, where Envoy
            checks policy and picks a READY endpoint.
          </p>
          <span className="flex shrink-0 gap-2">
            <AddServiceDialog />
            <Button size="sm" onClick={addNode} data-testid="add-node">
              <PlusIcon data-icon="inline-start" />
              add node
            </Button>
          </span>
        </div>

        <ControlPlaneStrip />
        <div className="boardbox relative overflow-hidden" ref={board.ref}>
          <div className="relative" style={{ height: layout.height }}>
            <PolicyArcs snapshot={snapshot} layout={layout} />
            <AnimatePresence>
              {nodes.map((node) => {
                const nl = layout.nodes[node.metadata.name]
                return nl ? (
                  <NodeCard
                    key={node.metadata.name}
                    node={node}
                    layout={nl}
                    count={(byNode.get(node.metadata.name) ?? []).length}
                  />
                ) : null
              })}
            </AnimatePresence>
            <AnimatePresence>
              {services.map((svc) => {
                const rect = layout.services[svc.metadata.name]
                const workload = Object.values(snapshot.workloads).find((wl) =>
                  selectorMatches(svc.spec.selector, wl.spec.template.labels),
                )
                return rect ? (
                  <ServiceCard
                    key={svc.metadata.name}
                    service={svc}
                    workload={workload}
                    rect={rect}
                    snapshot={snapshot}
                    highlighted={
                      highlight?.from === svc.metadata.name || highlight?.to === svc.metadata.name
                    }
                  />
                ) : null
              })}
            </AnimatePresence>
            {layout.pendingTray && (
              <div
                className="absolute rounded-lg border-2 border-dashed border-ghost px-3 py-1"
                style={{ ...rectStyle(layout.pendingTray) }}
                data-testid="pending-tray"
              >
                <span className="eyebrow">pending · nowhere schedulable</span>
              </div>
            )}
            <AnimatePresence>
              {Object.values(snapshot.instances).map((inst) => {
                const rect =
                  layout.nodes[inst.spec.node ?? '']?.slots[inst.metadata.name] ??
                  layout.pendingSlots[inst.metadata.name]
                return rect ? (
                  <InstanceChip key={inst.metadata.name} instance={inst} rect={rect} />
                ) : null
              })}
            </AnimatePresence>
            <TrafficDotLayer layoutRef={layoutRef} disabled={reduced} />
          </div>
        </div>
        {reduced && (
          <p className="mt-2 text-xs text-muted-foreground">
            Reduced motion is on, so calls appear in the live rail instead of as moving dots.
          </p>
        )}
      </div>

      <div className="flex min-h-0 flex-col gap-4 xl:sticky xl:top-16 xl:h-[calc(100vh-140px)] xl:overflow-y-auto">
        <TracePanel reduced={reduced} />
        <PolicyBuilder />
        <PolicySimPanel onHighlight={setHighlight} />
        <div className="min-h-0 flex-1">
          <TrafficRail />
        </div>
      </div>
    </div>
  )
}

function rectStyle(r: { x: number; y: number; w: number; h: number }) {
  return { left: r.x, top: r.y, width: r.w, height: r.h }
}
