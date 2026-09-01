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

  it('offers the facade machine LAN address when the browser only knows localhost', () => {
    const cmd = internetJoinCommand({ ...info, machineAddresses: ['192.168.1.20', '10.0.0.4'] })
    expect(cmd).toContain('--server 192.168.1.20:7443')
  })

  it('demands an advertise address so the new machine never guesses its Docker bridge', () => {
    expect(internetJoinCommand(info)).toContain('--advertise-address <new-machine-address>')
  })
})
