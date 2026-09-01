import { describe, expect, it } from 'bun:test'
import type { Instance } from '@/api/types'
import { pickNode, type SchedulableNode } from './scheduler'

function node(
  name: string,
  opts: {
    maxInstances?: number
    cordoned?: boolean
    phase?: 'Ready' | 'NotReady' | 'Draining'
    infraReady?: boolean
  } = {},
): SchedulableNode {
  return {
    node: {
      apiVersion: 'klite/v1',
      kind: 'Node',
      metadata: { name },
      spec: { maxInstances: opts.maxInstances ?? 32 },
      status: { phase: opts.phase ?? 'Ready', unschedulable: opts.cordoned, instanceCount: 0 },
    },
    infraReady: opts.infraReady ?? true,
  }
}

function placedOn(names: string[]): Instance[] {
  return names.map((n, i) => ({
    apiVersion: 'klite/v1',
    kind: 'Instance',
    metadata: { name: `x-${i}` },
    spec: { workload: 'x', node: n, container: { name: 'c', image: 'img' } },
    status: { phase: 'Ready', restarts: 0 },
  }))
}

describe('spread scheduling (ADR 0012)', () => {
  it('picks the emptiest node', () => {
    const nodes = [node('node-1'), node('node-2'), node('node-3')]
    expect(pickNode(undefined, nodes, placedOn(['node-1', 'node-1', 'node-2']))).toBe('node-3')
  })

  it('breaks ties by name', () => {
    const nodes = [node('node-2'), node('node-1')]
    expect(pickNode(undefined, nodes, [])).toBe('node-1')
  })

  it('skips cordoned, NotReady, and infra-less nodes', () => {
    const nodes = [
      node('node-1', { cordoned: true }),
      node('node-2', { phase: 'NotReady' }),
      node('node-3', { infraReady: false }),
      node('node-4'),
    ]
    expect(pickNode(undefined, nodes, [])).toBe('node-4')
  })

  it('respects maxInstances', () => {
    const nodes = [node('node-1', { maxInstances: 2 }), node('node-2')]
    expect(
      pickNode(undefined, nodes, placedOn(['node-1', 'node-1', 'node-2', 'node-2', 'node-2'])),
    ).toBe('node-2')
  })

  it('honors a pin, and leaves a pinned instance unscheduled if the pin is cordoned', () => {
    const nodes = [node('node-1'), node('node-2', { cordoned: true })]
    expect(pickNode('node-1', nodes, placedOn(['node-1', 'node-1', 'node-1']))).toBe('node-1')
    expect(pickNode('node-2', nodes, [])).toBeNull()
  })

  it('does not count Terminating instances toward spread', () => {
    const placed = placedOn(['node-1', 'node-2'])
    placed[0].status.phase = 'Terminating'
    const nodes = [node('node-1'), node('node-2')]
    expect(pickNode(undefined, nodes, placed)).toBe('node-1')
  })
})
