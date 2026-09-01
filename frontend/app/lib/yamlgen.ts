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

// The chatty demo container: busybox serves its own name over HTTP and
// calls one random other service through the real data path (kdns, VIP,
// Envoy). Calls come in waves: every instance sleeps to the same 10-second
// wall-clock boundary, then rolls one die, so the board launches a
// generation of dots together and they land with two seconds to spare
// before the next wave fires. 16384 of 65536 is 25 percent, which keeps
// the chosen 2.5 percent per second. TARGETS is baked at creation time.
export function chattyContainer(name: string, targets: string[]) {
  const script = [
    `echo "$(hostname) is ${name}" > /www/index.html`,
    '(httpd -f -vv -p 80 -h /www &)',
    // the sleep rides the do-line: a '; ' join would otherwise render 'do;',
    // which busybox ash rejects. sleep runs backgrounded under wait, since a
    // trap only fires between foreground commands and a drain shouldn't
    // stall out the rest of the sleep.
    'while :; do sleep $((10 - $(date +%s) % 10)) & wait $!',
    'r=$(head -c2 /dev/urandom | od -An -tu2 | tr -d " ")',
    '[ "$r" -lt 16384 ] || continue',
    'n=0; for t in $TARGETS; do n=$((n+1)); done',
    '[ "$n" -gt 0 ] || continue',
    'k=$(( ($(head -c2 /dev/urandom | od -An -tu2 | tr -d " ") % n) + 1 ))',
    'i=0; for t in $TARGETS; do i=$((i+1)); [ "$i" = "$k" ] && pick=$t; done',
    'wget -q -T 2 -O- "http://$pick:8080" >/dev/null 2>&1 && echo "send -> $pick ok" || echo "send -> $pick FAILED"',
    'done',
  ].join('; ')
  return {
    name: 'web',
    image: 'busybox:1.36',
    command: ['/bin/sh', '-c'],
    // busybox sh is PID 1 and drops SIGTERM without a handler, so the trap
    // comes first: without it every drain and scale-down waits the full
    // termination grace before the SIGKILL lands
    args: [`trap "exit 0" TERM; mkdir -p /www; ${script}`],
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
