import { describe, expect, it } from 'bun:test'
import { joinCommand } from './JoinNodeDialog'

describe('joinCommand', () => {
  it('keeps the local command lean: the default server already points home', () => {
    expect(joinCommand({ node: 'node-4', token: 'K10abc', endpoints: ['10.0.0.5:7443'] }, 'local')).toBe(
      'klite-agent --node node-4 --token K10abc',
    )
  })

  it('gives the internet command a routable server with the cluster port', () => {
    const cmd = joinCommand({ node: 'node-2', token: 't', endpoints: ['127.0.0.1:7443'] }, 'internet')
    expect(cmd).toContain('--server ')
    expect(cmd).toContain(':7443')
    expect(cmd).not.toContain('--server 127.0.0.1')
  })
})
