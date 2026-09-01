import { describe, expect, it } from 'bun:test'
import { internetJoinCommand, oneLinerJoinCommand } from './JoinNodeDialog'

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

describe('oneLinerJoinCommand', () => {
  it('pipes join.sh with the url, token, and node baked in', () => {
    const cmd = oneLinerJoinCommand({ ...info, machineAddresses: ['192.168.1.20'] })
    expect(cmd).toContain('releases/latest/download/join.sh')
    expect(cmd).toContain('KLITE_URL=192.168.1.20:7443')
    expect(cmd).toContain("KLITE_TOKEN='K10abc::node:dev-token'")
    expect(cmd).toContain('KLITE_NODE=node-4')
    expect(cmd).not.toContain('KLITE_VPN')
  })

  it('switches to tailscale mode for a tailnet cluster address', () => {
    const cmd = oneLinerJoinCommand({ ...info, tailnetAddress: '100.69.43.39' })
    expect(cmd).toContain('KLITE_URL=100.69.43.39:7443')
    expect(cmd).toContain('KLITE_VPN=tailscale')
    expect(cmd).toContain('KLITE_YES=1')
  })

  it('leaves plain 100.x addresses outside the CGNAT range alone', () => {
    const cmd = oneLinerJoinCommand({ ...info, machineAddresses: ['100.20.1.5'] })
    expect(cmd).not.toContain('KLITE_VPN')
  })
})
