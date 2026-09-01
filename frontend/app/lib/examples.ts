// These are the repo's own example policies, verbatim, plus two snippets the
// demo keeps reaching for. If they drift from examples/ in the repo, the demo
// breaks visibly, which is the point.

export const EXAMPLES: { label: string; yaml: string }[] = [
  {
    label: 'deny a → c',
    yaml: `apiVersion: klite/v1
kind: NetworkPolicy
metadata:
  name: deny-a-to-c
spec:
  action: DENY
  rules:
    - from: a
      to: c
`,
  },
  {
    label: 'allow only a → b (ALLOW-flip)',
    yaml: `# The moment this policy targets b, only matching callers reach b.
apiVersion: klite/v1
kind: NetworkPolicy
metadata:
  name: allow-only-a-to-b
spec:
  action: ALLOW
  rules:
    - from: a
      to: b
`,
  },
  {
    label: 'lockdown a (deny a → * except b)',
    yaml: `apiVersion: klite/v1
kind: NetworkPolicy
metadata:
  name: lockdown-a
spec:
  action: DENY
  rules:
    - from: a
      to: "*"
      except: [b]
`,
  },
  {
    label: 'scale b to 3',
    yaml: `apiVersion: klite/v1
kind: Workload
metadata:
  name: b
  labels:
    app: b
spec:
  replicas: 3
  template:
    labels:
      app: b
    containers:
      - name: web
        image: traefik/whoami:v1.10
        env:
          - name: WHOAMI_NAME
            value: b
        ports:
          - containerPort: 80
        readinessProbe:
          tcpPort: 80
`,
  },
  {
    label: 'add node-4',
    yaml: `apiVersion: klite/v1
kind: Node
metadata:
  name: node-4
  labels:
    zone: local
spec:
  maxInstances: 32
`,
  },
]
