// decodeObject must accept both wire dialects and produce identical canonical
// objects: the codec's user-facing JSON (lists, apply) and raw protojson
// (watch events). Fixtures below are shaped exactly like each producer's
// output — internal/object.EncodeJSON and protojson.Marshal of a WatchEvent.

import { describe, expect, it } from 'bun:test'
import { decodeObject } from './decode'

describe('decodeObject', () => {
  it('passes a codec-dialect object through unchanged', () => {
    const obj = decodeObject({
      apiVersion: 'klite/v1',
      kind: 'Instance',
      metadata: { name: 'b-1', resourceVersion: 41 },
      spec: { workload: 'b', node: 'node-2' },
      status: { phase: 'INSTANCE_PHASE_READY', restarts: 0, instanceIp: '10.44.128.3' },
    })
    expect(obj?.kind).toBe('Instance')
    expect(obj?.metadata.name).toBe('b-1')
    expect(obj?.kind === 'Instance' && obj.status.phase).toBe('Ready')
  })

  it('unwraps the protojson oneof, meta key, and enum names', () => {
    const obj = decodeObject({
      node: {
        meta: { name: 'node-1', resourceVersion: '77' },
        spec: { maxInstances: 32 },
        status: { phase: 'NODE_PHASE_NOT_READY', unschedulable: true, instanceCount: 2 },
      },
    })
    expect(obj?.kind).toBe('Node')
    expect(obj?.metadata.resourceVersion).toBe(77) // protojson int64 arrives as a string
    expect(obj?.kind === 'Node' && obj.status?.phase).toBe('NotReady')
    expect(obj?.kind === 'Node' && obj.status?.unschedulable).toBe(true)
  })

  it('normalizes policy actions from both dialects', () => {
    const fromWatch = decodeObject({
      networkPolicy: {
        meta: { name: 'lockdown-a' },
        spec: { action: 'POLICY_ACTION_DENY', rules: [{ from: '*', to: 'a' }] },
      },
    })
    const fromList = decodeObject({
      kind: 'NetworkPolicy',
      metadata: { name: 'lockdown-a' },
      spec: { action: 'DENY', rules: [{ from: '*', to: 'a' }] },
    })
    expect(fromWatch?.kind === 'NetworkPolicy' && fromWatch.spec.action).toBe('DENY')
    expect(fromList?.kind === 'NetworkPolicy' && fromList.spec.action).toBe('DENY')
  })

  it('decodes a VIPAllocation from the watch dialect', () => {
    const obj = decodeObject({
      vipAllocation: {
        meta: { name: 'b.node-2' },
        spec: { service: 'b', node: 'node-2', vip: '10.44.64.7' },
      },
    })
    expect(obj?.kind).toBe('VIPAllocation')
    expect(obj?.kind === 'VIPAllocation' && obj.spec.vip).toBe('10.44.64.7')
  })

  it('returns null for payloads without a recognizable kind', () => {
    expect(decodeObject(null)).toBeNull()
    expect(decodeObject({ something: 'else' })).toBeNull()
    expect(decodeObject('nope')).toBeNull()
  })
})

describe('protojson zero-value omission', () => {
  it('fills the holes the app walks unconditionally', () => {
    const svc = decodeObject({ service: { meta: { name: 'bare' }, spec: {} } })
    expect(svc?.kind === 'Service' && svc.spec.selector).toEqual({})
    const pol = decodeObject({
      networkPolicy: { meta: { name: 'p' }, spec: { action: 'POLICY_ACTION_DENY' } },
    })
    expect(pol?.kind === 'NetworkPolicy' && pol.spec.rules).toEqual([])
    const wl = decodeObject({ workload: { meta: { name: 'w' }, spec: {} } })
    expect(wl?.kind === 'Workload' && wl.spec.replicas).toBe(0)
    expect(wl?.kind === 'Workload' && wl.spec.template.labels).toEqual({})
    const node = decodeObject({
      node: {
        meta: { name: 'n' },
        spec: { maxInstances: 32 },
        status: { phase: 'NODE_PHASE_READY', lastHeartbeatUnix: '1756000000', nodeIndex: '2' },
      },
    })
    expect(node?.kind === 'Node' && node.status?.instanceCount).toBe(0)
    expect(node?.kind === 'Node' && node.status?.lastHeartbeatUnix).toBe(1756000000)
    expect(node?.kind === 'Node' && node.status?.nodeIndex).toBe(2)
  })
})
