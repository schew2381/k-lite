// The static policy edges (ADR 0015): red arcs with an ✕ for DENY rules,
// green for ALLOW, drawn between service cards in the SVG under-layer.

import { useMemo } from 'react'
import type { Layout, Point } from '@/layout/layout'
import { sortedPolicies, sortedServices } from '@/store/selectors'
import type { Snapshot } from '@/store/store'

interface Arc {
  key: string
  from: Point
  to: Point
  action: 'ALLOW' | 'DENY'
  label: string
}

function resolve(sel: string, all: string[]): string[] {
  return sel === '*' ? all : all.includes(sel) ? [sel] : []
}

export function PolicyArcs({ snapshot, layout }: { snapshot: Snapshot; layout: Layout }) {
  const arcs = useMemo(() => {
    const names = sortedServices(snapshot).map((s) => s.metadata.name)
    const out: Arc[] = []
    for (const p of sortedPolicies(snapshot)) {
      for (const [i, rule] of p.spec.rules.entries()) {
        for (const from of resolve(rule.from, names)) {
          for (const to of resolve(rule.to, names)) {
            if (from === to || rule.except?.includes(to)) continue
            const a = layout.services[from]
            const b = layout.services[to]
            if (!a || !b) continue
            out.push({
              key: `${p.metadata.name}:${i}:${from}:${to}`,
              // arc from top edge to top edge, bowing into the band above
              from: { x: a.x + a.w / 2, y: a.y - 2 },
              to: { x: b.x + b.w / 2, y: b.y - 2 },
              action: p.spec.action,
              label: p.metadata.name,
            })
          }
        }
      }
    }
    return out
  }, [snapshot, layout])

  if (arcs.length === 0) return null

  return (
    <svg
      className="pointer-events-none absolute inset-0"
      width={layout.width}
      height={layout.height}
      aria-hidden
    >
      {arcs.map((arc) => {
        const midX = (arc.from.x + arc.to.x) / 2
        const topY = Math.min(arc.from.y, arc.to.y)
        const peakY = topY - 36 // inside the band computeLayout reserves
        const color = arc.action === 'DENY' ? 'var(--destructive)' : 'var(--success)'
        return (
          <g key={arc.key}>
            <path
              d={`M ${arc.from.x} ${arc.from.y} Q ${midX} ${peakY - 14} ${arc.to.x} ${arc.to.y}`}
              fill="none"
              stroke={color}
              strokeWidth={2}
              strokeDasharray={arc.action === 'DENY' ? '6 5' : undefined}
              opacity={0.85}
            />
            {arc.action === 'DENY' && (
              <g stroke={color} strokeWidth={3.2} strokeLinecap="round">
                <line x1={midX - 6} y1={peakY - 12} x2={midX + 6} y2={peakY} />
                <line x1={midX + 6} y1={peakY - 12} x2={midX - 6} y2={peakY} />
              </g>
            )}
            <text x={midX + 12} y={peakY - 4} fontSize={10.5} fill={color} fontFamily="var(--font-mono)">
              {arc.label}
            </text>
          </g>
        )
      })}
    </svg>
  )
}
