import { describe, expect, it } from 'bun:test'
import { joinCommand } from './JoinNodeDialog'

describe('joinCommand', () => {
  it('names the node, every endpoint, and the token', () => {
    expect(
      joinCommand({ node: 'node-4', token: 'K10abc', endpoints: ['10.0.0.5:7443', '10.0.0.6:7443'] }),
    ).toBe('klite-agent --node node-4 --server 10.0.0.5:7443,10.0.0.6:7443 --token K10abc')
  })

  it('falls back to the default server when the facade reports none', () => {
    expect(joinCommand({ node: 'node-2', token: 't', endpoints: [] })).toContain(
      '--server 127.0.0.1:7443',
    )
  })
})
