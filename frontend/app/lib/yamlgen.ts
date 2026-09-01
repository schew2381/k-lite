// Every UI mutation that isn't a real RPC goes through apply(). These
// helpers keep it honest: all they can emit is a YAML document.

import { stringify } from 'yaml'
import type { KliteObject } from '@/api/types'

function toYaml(obj: KliteObject): string {
  // status is server-owned, so a client never applies it
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

// The chatty demo container: busybox serves its own name over HTTP and rolls
// a five-percent die every second to wget one random other service through
// the real data path (kdns, VIP, Envoy). TARGETS is baked at creation time.
function chattyContainer(name: string, targets: string[]) {
  const script = [
    `echo "$(hostname) is ${name}" > /www/index.html`,
    'httpd -p 80 -h /www',
    // the roll rides the do-line: a '; ' join would otherwise render 'do;',
    // which busybox ash rejects. 0..65535 from urandom, under 3277 is five percent
    'while sleep 1; do r=$(head -c2 /dev/urandom | od -An -tu2 | tr -d " ")',
    '[ "$r" -lt 3277 ] || continue',
    'n=0; for t in $TARGETS; do n=$((n+1)); done',
    '[ "$n" -gt 0 ] || continue',
    'k=$(( ($(head -c2 /dev/urandom | od -An -tu2 | tr -d " ") % n) + 1 ))',
    'i=0; for t in $TARGETS; do i=$((i+1)); [ "$i" = "$k" ] && pick=$t; done',
    'wget -q -T 2 -O- "http://$pick:8080" >/dev/null 2>&1 && echo "-> $pick ok" || echo "-> $pick FAILED"',
    'done',
  ].join('; ')
  return {
    name: 'web',
    image: 'busybox:1.36',
    command: ['/bin/sh', '-c'],
    args: [`mkdir -p /www; ${script}`],
    env: [{ name: 'TARGETS', value: targets.join(' ') }],
    ports: [{ containerPort: 80 }],
    readinessProbe: { tcpPort: 80 },
  }
}

export function newServiceYaml(name: string, targets: string[], replicas: number): string {
  const workload: KliteObject = {
    apiVersion: 'klite/v1',
    kind: 'Workload',
    metadata: { name, labels: { app: name } },
    spec: {
      replicas,
      template: {
        labels: { app: name },
        containers: [chattyContainer(name, targets)],
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
