// This is the "can A reach B?" widget from ADR 0015. The verdict comes from
// client.policyCheck, which runs the same evaluator as the data path, so what
// this panel says is what the proxy does.

import { useCallback, useEffect, useRef, useState } from 'react'
import type { Verdict } from '@/api/types'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import { sortedServices } from '@/store/selectors'
import { useSnapshot } from '@/store/store'

const REASON_COPY: Record<Verdict['reason'], string> = {
  'default-allow': 'no ALLOW policy targets it, so the default says yes',
  'allow-rule': 'an ALLOW rule matches this caller',
  'deny-rule': 'a DENY rule matches, so the connection resets',
  'no-allow-match': "an ALLOW policy targets it and this caller isn't on the list",
}

export function PolicySimPanel({
  onHighlight,
}: {
  onHighlight: (pair: { from: string; to: string } | null) => void
}) {
  const client = useClient()
  const snapshot = useSnapshot()
  const services = sortedServices(snapshot).map((s) => s.metadata.name)
  const [from, setFrom] = useState<string>()
  const [to, setTo] = useState<string>()
  const [verdict, setVerdict] = useState<Verdict | null>(null)

  // changing either end voids the shown verdict and the board highlight
  const pick = (set: (v: string) => void) => (v: string) => {
    set(v)
    setVerdict(null)
    onHighlight(null)
  }

  const check = useCallback(async () => {
    if (!from || !to) return
    setVerdict(await client.policyCheck(from, to))
    onHighlight({ from, to })
  }, [client, from, to, onHighlight])

  // policies change under us, so a shown verdict must stay honest
  const lastPolicies = useRef(snapshot.policies)
  useEffect(() => {
    if (lastPolicies.current === snapshot.policies) return
    lastPolicies.current = snapshot.policies
    if (verdict) act(check())
  })

  useEffect(() => () => onHighlight(null), [onHighlight])

  return (
    <div className="boardbox flex flex-col gap-2.5 p-3" data-testid="policy-sim">
      <div className="eyebrow">can A reach B?</div>
      <div className="flex items-center gap-2">
        <Select value={from} onValueChange={pick(setFrom)}>
          <SelectTrigger size="sm" className="flex-1" aria-label="Caller service">
            <SelectValue placeholder="caller" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {services.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <span className="text-muted-foreground">→</span>
        <Select value={to} onValueChange={pick(setTo)}>
          <SelectTrigger size="sm" className="flex-1" aria-label="Destination service">
            <SelectValue placeholder="destination" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {services.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <Button size="sm" onClick={() => act(check())} disabled={!from || !to}>
          check
        </Button>
      </div>
      {verdict && (
        <div
          className={cn(
            'rounded-lg border-[1.5px] px-3 py-2',
            verdict.allowed ? 'border-traffic bg-traffic/10' : 'border-deny bg-deny/10',
          )}
          data-testid="policy-verdict"
          data-allowed={verdict.allowed}
        >
          <div
            className={cn('font-mono text-sm font-bold', verdict.allowed ? 'text-traffic' : 'text-deny')}
          >
            {verdict.allowed ? '✓ allowed' : '✕ denied'}
          </div>
          <div className="mt-0.5 text-xs text-muted-foreground">{REASON_COPY[verdict.reason]}</div>
          {verdict.matchedRule && (
            <div className="mt-1 font-mono text-[11px]">
              {verdict.matchedRule.policy} · rule {verdict.matchedRule.ruleIndex + 1} ·{' '}
              {verdict.matchedRule.action}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
