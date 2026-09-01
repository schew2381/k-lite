// pickNode does the spread scheduling of ADR 0012: filter, then take the
// emptiest node. Binding is the caller's job, and this function only chooses.

import type { Instance, NodeObj } from '@/api/types'

export interface SchedulableNode {
  node: NodeObj
  infraReady: boolean
}

export function pickNode(
  pin: string | undefined,
  nodes: SchedulableNode[],
  placed: Instance[],
): string | null {
  const counts = new Map<string, number>()
  for (const i of placed) {
    if (!i.spec.node) continue
    if (i.status.phase === 'Terminating') continue
    counts.set(i.spec.node, (counts.get(i.spec.node) ?? 0) + 1)
  }

  const candidates = nodes
    .filter(({ node, infraReady }) => {
      const name = node.metadata.name
      if (pin && pin !== name) return false
      if (node.status?.phase !== 'Ready') return false
      if (node.status?.unschedulable) return false
      if (!infraReady) return false
      if ((counts.get(name) ?? 0) >= node.spec.maxInstances) return false
      return true
    })
    .map(({ node }) => node.metadata.name)
    // fewest instances wins, and ties break by name, so placement is explainable
    .sort((a, b) => (counts.get(a) ?? 0) - (counts.get(b) ?? 0) || a.localeCompare(b))

  return candidates[0] ?? null
}
