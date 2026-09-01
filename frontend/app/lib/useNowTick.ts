// useNowTick re-renders on a wall-clock beat, for labels like "beat 2s ago"
// that age even when no store event arrives.

import { useEffect, useState } from 'react'

export function useNowTick(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(t)
  }, [intervalMs])
  return now
}
