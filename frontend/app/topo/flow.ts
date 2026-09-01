// useFlow picks between ADR 0027's two traffic flows. Traced walks one
// request step by step at the mock's teaching pace, and live plays every
// call at real speed. The flow follows the data source (mock gets traced and
// a live cluster gets live) until the header toggle or a ?flow= link pins it.

import { useSearchParams } from 'react-router-dom'
import { useClient } from '@/lib/client-context'

export type FlowMode = 'traced' | 'live'

export function useFlow(): FlowMode {
  const client = useClient()
  const [searchParams] = useSearchParams()
  const param = searchParams.get('flow')
  if (param === 'live' || param === 'traced') return param
  return client.mode === 'http' ? 'live' : 'traced'
}
