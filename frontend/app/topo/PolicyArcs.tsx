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
  level: number // stagger index per policy, so arcs and labels never share an apex
}

function resolve(sel: string, all: string[]): string[] {
  return sel === '*' ? all : all.includes(sel) ? [sel] : []
}

export function PolicyArcs({ snapshot, layout }: { snapshot: Snapshot; layout: Layout }) {
  const arcs = useMemo(() => {
    const names = sortedServices(snapshot).map((s) => s.metadata.name)
    const out: Arc[] = []
    for (const [level, p] of sortedPolicies(snapshot).entries()) {
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
              level,
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
      <defs>
        <marker
          id="arrow-allow"
          viewBox="0 0 10 10"
          refX="8"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--success)" />
        </marker>
        <marker
          id="arrow-deny"
          viewBox="0 0 10 10"
          refX="8"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--destructive)" />
        </marker>
      </defs>
      {arcs.map((arc) => {
        const midX = (arc.from.x + arc.to.x) / 2
        const topY = Math.min(arc.from.y, arc.to.y)
        // each policy peaks on its own level, so labels keep clear of each other
        const peakY = Math.max(14, topY - 36 - arc.level * 22)
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
              markerEnd={arc.action === 'DENY' ? 'url(#arrow-deny)' : 'url(#arrow-allow)'}
            />
            {arc.action === 'DENY' && (
              <g stroke={color} strokeWidth={3.2} strokeLinecap="round">
                <line x1={midX - 5} y1={peakY - 10} x2={midX + 5} y2={peakY} />
                <line x1={midX + 5} y1={peakY - 10} x2={midX - 5} y2={peakY} />
              </g>
            )}
            <text
              x={midX + (arc.action === 'DENY' ? 11 : 6)}
              y={peakY - 2}
              fontSize={10.5}
              fill={color}
              fontFamily="var(--font-mono)"
            >
              {arc.label}
            </text>
          </g>
        )
      })}
    </svg>
  )
}
