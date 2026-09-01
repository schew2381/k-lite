// Every UI mutation that isn't a real RPC goes through apply(). These
// helpers keep it honest: all they can emit is a YAML document.

import { stringify } from 'yaml'
import type { KliteObject } from '@/api/types'

function toYaml(obj: KliteObject): string {
  // status is server-owned; a client never applies it
  const { status: _status, ...rest } = obj as KliteObject & { status?: unknown }
  return stringify(rest)
}

export function newNodeYaml(name: string): string {
  return toYaml({
    apiVersion: 'klite/v1',
    kind: 'Node',
    metadata: { name, labels: { zone: 'local' } },
    spec: { maxInstances: 32 },
  })
}

export function newServiceYaml(name: string, image: string, replicas: number): string {
  const workload: KliteObject = {
    apiVersion: 'klite/v1',
    kind: 'Workload',
    metadata: { name, labels: { app: name } },
    spec: {
      replicas,
      template: {
        labels: { app: name },
        containers: [
          {
            name: 'web',
            image,
            env: [{ name: 'WHOAMI_NAME', value: name }],
            ports: [{ containerPort: 80 }],
            readinessProbe: { tcpPort: 80 },
          },
        ],
      },
    },
  }
  const service: KliteObject = {
    apiVersion: 'klite/v1',
    kind: 'Service',
    metadata: { name },
    spec: { selector: { app: name }, port: 8080, targetPort: 80 },
  }
  return `${toYaml(workload)}---\n${toYaml(service)}`
}

export function policyYaml(name: string, action: 'ALLOW' | 'DENY', from: string, to: string): string {
  return toYaml({
    apiVersion: 'klite/v1',
    kind: 'NetworkPolicy',
    metadata: { name },
    spec: { action, rules: [{ from, to }] },
  })
}
