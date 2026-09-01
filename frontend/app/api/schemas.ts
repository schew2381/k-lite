// Zod validates everything the apply box accepts against schemas cut from
// the protos, so a document that passes here is one the real backend will
// accept, with error messages that name the exact field.

import { parseAllDocuments } from 'yaml'
import { z } from 'zod'
import type { ApplyResult, KliteObject } from './types'

const meta = z.object({
  name: z
    .string()
    .min(1)
    .max(63)
    .regex(/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/, 'lowercase alphanumerics and dashes'),
  labels: z.record(z.string(), z.string()).optional(),
  uid: z.string().optional(),
  resourceVersion: z.number().optional(),
  createdUnix: z.number().optional(),
})

const container = z.object({
  name: z.string().min(1),
  image: z.string().min(1),
  command: z.array(z.string()).optional(),
  args: z.array(z.string()).optional(),
  env: z.array(z.object({ name: z.string(), value: z.string() })).optional(),
  ports: z.array(z.object({ containerPort: z.number().int().positive() })).optional(),
  readinessProbe: z.object({ tcpPort: z.number().int().positive() }).optional(),
  resources: z.object({ cpus: z.string().optional(), memory: z.string().optional() }).optional(),
})

const seconds = z.number().int().positive()

export const workloadSchema = z.object({
  apiVersion: z.literal('klite/v1'),
  kind: z.literal('Workload'),
  metadata: meta,
  spec: z.object({
    replicas: z.number().int().min(0).max(64),
    nodeName: z.string().optional(),
    template: z.object({
      labels: z.record(z.string(), z.string()),
      containers: z
        .array(container)
        .length(1, 'v1 allows exactly one container per Instance (ADR 0014)'),
    }),
    drain: z
      .object({ drainTimeoutSeconds: seconds.optional(), terminationGraceSeconds: seconds.optional() })
      .optional(),
  }),
})

export const serviceSchema = z.object({
  apiVersion: z.literal('klite/v1'),
  kind: z.literal('Service'),
  metadata: meta,
  spec: z.object({
    selector: z.record(z.string(), z.string()),
    port: z.number().int().positive(),
    targetPort: z.number().int().positive(),
  }),
  status: z.object({ vips: z.record(z.string(), z.string()) }).optional(),
})

export const nodeSchema = z.object({
  apiVersion: z.literal('klite/v1'),
  kind: z.literal('Node'),
  metadata: meta,
  spec: z.object({
    maxInstances: z.number().int().positive(),
  }),
  status: z.unknown().optional(),
})

export const policySchema = z.object({
  apiVersion: z.literal('klite/v1'),
  kind: z.literal('NetworkPolicy'),
  metadata: meta,
  spec: z.object({
    action: z.enum(['ALLOW', 'DENY']),
    rules: z
      .array(
        z
          .object({
            from: z.string().min(1),
            to: z.string().min(1),
            except: z.array(z.string()).optional(),
          })
          // matches the server: carve-outs only make sense against a wildcard
          .refine((r) => !r.except || r.to === '*', {
            message: 'except requires to: "*"',
          }),
      )
      .min(1),
  }),
})

const anyObject = z.discriminatedUnion('kind', [workloadSchema, serviceSchema, nodeSchema, policySchema])

export interface ParsedApply {
  objects: KliteObject[]
  errors: ApplyResult[]
}

// Multi-document YAML in, validated objects and per-document errors out.
function nameOf(raw: Record<string, unknown>): string {
  if (typeof raw.metadata === 'object' && raw.metadata !== null && 'name' in raw.metadata) {
    return String((raw.metadata as { name: unknown }).name)
  }
  return '(unnamed)'
}

export function parseApplyYaml(text: string): ParsedApply {
  const objects: KliteObject[] = []
  const errors: ApplyResult[] = []
  const docs = parseAllDocuments(text)
  for (const doc of docs) {
    if (doc.errors.length > 0) {
      errors.push({
        kind: 'Workload',
        name: '(unparsed)',
        action: 'error',
        error: doc.errors[0].message,
      })
      continue
    }
    const raw = doc.toJS() as Record<string, unknown> | null
    if (raw == null) continue // blank document between separators
    if (raw.kind === 'Instance' || raw.kind === 'VIPAllocation' || raw.kind === 'IngressAllocation') {
      // server-materialized kinds, mirroring klited's apply rejection (ADR 0022)
      errors.push({
        kind: raw.kind,
        name: nameOf(raw),
        action: 'error',
        error: `${raw.kind} is server-materialized, so apply can't create one`,
      })
      continue
    }
    const result = anyObject.safeParse(raw)
    if (result.success) {
      objects.push(result.data as KliteObject)
    } else {
      const issue = result.error.issues[0]
      const kind = typeof raw.kind === 'string' ? (raw.kind as ApplyResult['kind']) : 'Workload'
      errors.push({
        kind,
        name: nameOf(raw),
        action: 'error',
        error: `${issue.path.join('.') || 'document'}: ${issue.message}`,
      })
    }
  }
  return { objects, errors }
}
