import { describe, expect, it } from 'bun:test'
import { internetJoinCommand } from './JoinNodeDialog'

const info = {
  node: 'node-4',
  token: 'K10abc::node:dev-token',
  endpoints: ['127.0.0.1:7443'],
}

describe('internetJoinCommand', () => {
  it('never hands a remote machine a loopback server address', () => {
    const cmd = internetJoinCommand(info)
    expect(cmd).toContain('--node node-4')
    expect(cmd).toContain('--token K10abc::node:dev-token')
    expect(cmd).toContain('--server')
    expect(cmd).not.toContain('--server 127.0.0.1')
    expect(cmd).not.toContain('--server localhost')
  })

  it('keeps the klited port from the cluster endpoints', () => {
    expect(internetJoinCommand(info)).toContain(':7443')
  })
})
