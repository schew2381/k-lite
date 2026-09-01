// A live cluster moves through Pending and Running in ~180ms, which renders
// as nothing. useDwelledPhase lets the display lag the truth just enough to
// read: each observed phase holds for at least the dwell before the next one
// shows, and slow transitions pass through untouched since their wait
// computes to zero. Actions must key off the actual phase, never this one.

import { useEffect, useRef, useState } from 'react'

export const DWELL_MS = 1000 // a full second per phase, so a live birth reads as three deliberate beats

export function useDwelledPhase<T>(actual: T, dwellMs = DWELL_MS): T {
  const [shown, setShown] = useState(actual)
  const shownRef = useRef(actual)
  const shownSince = useRef(performance.now())
  const queue = useRef<T[]>([])
  const timer = useRef(0)

  useEffect(() => {
    const tail = queue.current.length > 0 ? queue.current[queue.current.length - 1] : shownRef.current
    if (actual === tail) return
    queue.current.push(actual)

    const pump = () => {
      if (timer.current) return
      const wait = Math.max(0, dwellMs - (performance.now() - shownSince.current))
      timer.current = window.setTimeout(() => {
        timer.current = 0
        const next = queue.current.shift()
        if (next === undefined) return
        shownRef.current = next
        shownSince.current = performance.now()
        setShown(next)
        if (queue.current.length > 0) pump()
      }, wait)
    }
    pump()
  }, [actual])

  useEffect(
    () => () => {
      window.clearTimeout(timer.current)
    },
    [],
  )

  return shown
}
