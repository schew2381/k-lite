import { describe, expect, it } from 'bun:test'
import type { NodeObj } from '@/api/types'
import { agentViewOf, HEARTBEAT_FRESH_MS } from './selectors'

const node = (status?: NodeObj['status']): NodeObj =>
  ({
    apiVersion: 'klite/v1',
    kind: 'Node',
    metadata: { name: 'node-1' },
    spec: { maxInstances: 32 },
    status,
  }) as NodeObj

const NOW = 1_788_000_000_000

describe('agentViewOf', () => {
  it('calls a node with no status a waiting machine', () => {
    expect(agentViewOf(node(undefined), false, NOW).state).toBe('waiting')
  })

  it('trusts phase in mock, where the sim pauses with the page', () => {
    expect(agentViewOf(node({ phase: 'Ready' } as NodeObj['status']), true, NOW).state).toBe('running')
    expect(agentViewOf(node({ phase: 'NotReady' } as NodeObj['status']), true, NOW).state).toBe('gone')
  })

  it('trusts the heartbeat live, fresh then stale', () => {
    const fresh = agentViewOf(
      node({ phase: 'Ready', lastHeartbeatUnix: (NOW - 2_000) / 1000 } as NodeObj['status']),
      false,
      NOW,
    )
    expect(fresh.state).toBe('running')
    expect(fresh.beatAgoMs).toBe(2_000)
    const stale = agentViewOf(
      node({
        phase: 'Ready',
        lastHeartbeatUnix: (NOW - HEARTBEAT_FRESH_MS - 1_000) / 1000,
      } as NodeObj['status']),
      false,
      NOW,
    )
    expect(stale.state).toBe('gone')
  })

  it('treats a live status without any heartbeat as waiting', () => {
    expect(agentViewOf(node({ phase: 'NotReady' } as NodeObj['status']), false, NOW).state).toBe(
      'waiting',
    )
  })
})
