// Buttons fire promises and move on. act() makes sure a failure surfaces as a
// toast instead of an unhandled rejection — invisible with the mock, vital
// against a real backend.

import { toast } from 'sonner'

export function act(promise: Promise<unknown>): void {
  promise.catch((err: unknown) => {
    toast.error(err instanceof Error ? err.message : String(err))
  })
}
