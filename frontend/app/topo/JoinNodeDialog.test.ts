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
    expect(cmd).toContain("--token 'K10abc::node:dev-token'")
    expect(cmd).toContain('--server')
    expect(cmd).not.toContain('--server 127.0.0.1')
    expect(cmd).not.toContain('--server localhost')
  })

  it('keeps the klited port from the cluster endpoints', () => {
    expect(internetJoinCommand(info)).toContain(':7443')
  })

  it('prefers the tailnet address over everything, then the LAN address', () => {
    const both = internetJoinCommand({
      ...info,
      machineAddresses: ['192.168.1.20'],
      tailnetAddress: '100.69.43.39',
    })
    expect(both).toContain('--server 100.69.43.39:7443')
    const lanOnly = internetJoinCommand({ ...info, machineAddresses: ['192.168.1.20', '10.0.0.4'] })
    expect(lanOnly).toContain('--server 192.168.1.20:7443')
  })

  it('is paste-ready: ./klite-agent, quoted token, advertise resolved at paste time', () => {
    const cmd = internetJoinCommand({ ...info, tailnetAddress: '100.69.43.39' })
    expect(cmd.startsWith('./klite-agent ')).toBe(true)
    expect(cmd).toContain('--advertise-address "$(tailscale ip -4)"')
    expect(cmd).not.toContain('<')
  })
})
