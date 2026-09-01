// These sim timings compress the real design values (30s drain, 15s grace in
// ADR 0010) to demo pace. Everything is injectable, so tests pin exact values,
// and the drain block on a Workload overrides the defaults.

export interface SimTimings {
  containerStartMs: number
  probeMs: number
  infraStartMs: number
  heartbeatMs: number
  missedHeartbeatsForNotReady: number
  rescheduleGraceMs: number
  drainTimeoutMs: number
  terminationGraceMs: number
  trafficPeriodMs: number
  restartBackoffMs: number[]
  backoffResetAfterMs: number
}

export const DEFAULT_TIMINGS: SimTimings = {
  containerStartMs: 400,
  probeMs: 1500,
  infraStartMs: 1000,
  heartbeatMs: 1000,
  missedHeartbeatsForNotReady: 5,
  rescheduleGraceMs: 8000,
  drainTimeoutMs: 4000,
  terminationGraceMs: 2000,
  trafficPeriodMs: 4000, // the played story skips beats while it runs, then the next call is at most one beat away
  restartBackoffMs: [500, 1000, 2000, 4000],
  backoffResetAfterMs: 60000,
}
