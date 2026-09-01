// Each instance gets a synthetic log ring. The traffic generator writes both
// the caller's and the destination's lines, so the Logs page narrates exactly
// what the dots show.

import type { LogLine } from '@/api/types'

const CAP = 500

export class LogStore {
  private rings = new Map<string, LogLine[]>()
  private followers = new Map<string, Set<(l: LogLine) => void>>()

  push(instance: string, ts: number, line: string) {
    let ring = this.rings.get(instance)
    if (!ring) {
      ring = []
      this.rings.set(instance, ring)
    }
    const entry = { ts, line }
    ring.push(entry)
    if (ring.length > CAP) ring.shift()
    const subs = this.followers.get(instance)
    if (subs) for (const cb of subs) cb(entry)
  }

  subscribe(instance: string, cb: (l: LogLine) => void): () => void {
    for (const entry of this.rings.get(instance) ?? []) cb(entry)
    let subs = this.followers.get(instance)
    if (!subs) {
      subs = new Set()
      this.followers.set(instance, subs)
    }
    subs.add(cb)
    return () => subs.delete(cb)
  }

  drop(instance: string) {
    this.rings.delete(instance)
    this.followers.delete(instance)
  }
}
