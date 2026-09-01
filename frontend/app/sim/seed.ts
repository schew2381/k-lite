// The default cluster mirrors examples/seed in the repo: four nodes, the
// chatty a/b/c/d workloads behind services at 1/2/3/2, and the two policies
// the live playground enforces at rest. only-a-reaches-d flips d onto an
// allowlist and deny-c-to-b kills one pair outright, so the resting board
// shows denied chatter in both modes. The simulator generates the calls
// itself: every Ready instance dials the services in its own rotation.

import type { KliteObject, NetworkPolicy, Service, Workload } from '@/api/types'
import { chattyContainer } from '@/lib/yamlgen'

function node(name: string): KliteObject {
  return {
    apiVersion: 'klite/v1',
    kind: 'Node',
    metadata: { name, labels: { zone: 'local' } },
    spec: { maxInstances: 32 },
  }
}

function service(name: string): Service {
  return {
    apiVersion: 'klite/v1',
    kind: 'Service',
    metadata: { name },
    spec: { selector: { app: name }, port: 8080, targetPort: 80 },
  }
}

function chatty(name: string, replicas: number, targets: string[]): Workload {
  return {
    apiVersion: 'klite/v1',
    kind: 'Workload',
    metadata: { name, labels: { app: name } },
    spec: {
      replicas,
      template: { labels: { app: name }, containers: [chattyContainer(name, targets)] },
    },
  }
}

function policy(name: string, action: 'ALLOW' | 'DENY', from: string, to: string): NetworkPolicy {
  return {
    apiVersion: 'klite/v1',
    kind: 'NetworkPolicy',
    metadata: { name },
    spec: { action, rules: [{ from, to }] },
  }
}

const NAMES = ['a', 'b', 'c', 'd']
const REPLICAS: Record<string, number> = { a: 1, b: 2, c: 3, d: 2 }

export function seedObjects(): KliteObject[] {
  const out: KliteObject[] = [node('node-1'), node('node-2'), node('node-3'), node('node-4')]
  for (const name of NAMES) {
    out.push(
      chatty(
        name,
        REPLICAS[name],
        NAMES.filter((t) => t !== name),
      ),
    )
    out.push(service(name))
  }
  out.push(policy('only-a-reaches-d', 'ALLOW', 'a', 'd'))
  out.push(policy('deny-c-to-b', 'DENY', 'c', 'b'))
  return out
}
