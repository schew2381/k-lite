// PolicyBuilder writes a named policy without leaving the board: pick who may
// (or may not) talk to whom, name it, apply. Denials then show up in the rail
// and in the caller's log as "blocked by {name}".

import { Trash2Icon } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
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
import { policyYaml } from '@/lib/yamlgen'
import { sortedPolicies, sortedServices } from '@/store/selectors'
import { useSnapshot } from '@/store/store'

export function PolicyBuilder() {
  const client = useClient()
  const snapshot = useSnapshot()
  const services = sortedServices(snapshot).map((s) => s.metadata.name)
  const policies = sortedPolicies(snapshot)

  const [action, setAction] = useState<'DENY' | 'ALLOW'>('DENY')
  const [from, setFrom] = useState<string>()
  const [to, setTo] = useState<string>()
  const [name, setName] = useState('')

  const suggested =
    from && to
      ? `${action.toLowerCase()}-${from === '*' ? 'all' : from}-to-${to === '*' ? 'all' : to}`
      : ''
  const effective = name.trim() || suggested

  const apply = () => {
    if (!from || !to || !effective) return
    act(
      client.apply(policyYaml(effective, action, from, to)).then((results) => {
        const failed = results.find((r) => r.action === 'error')
        if (failed) toast.error(failed.error ?? 'policy rejected')
        else {
          toast(`policy ${effective} applied`)
          setName('')
        }
      }),
    )
  }

  return (
    <div className="boardbox flex flex-col gap-2.5 p-3" data-testid="policy-builder">
      <div className="eyebrow">policies</div>
      <div className="flex items-center gap-1.5">
        <Select value={action} onValueChange={(v) => setAction(v as 'DENY' | 'ALLOW')}>
          <SelectTrigger size="sm" className="w-24" aria-label="Policy action">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              <SelectItem value="DENY">DENY</SelectItem>
              <SelectItem value="ALLOW">ALLOW</SelectItem>
            </SelectGroup>
          </SelectContent>
        </Select>
        <Select value={from} onValueChange={setFrom}>
          <SelectTrigger size="sm" className="flex-1" aria-label="Caller">
            <SelectValue placeholder="from" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              <SelectItem value="*">* (any)</SelectItem>
              {services.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <span className="text-muted-foreground">→</span>
        <Select value={to} onValueChange={setTo}>
          <SelectTrigger size="sm" className="flex-1" aria-label="Destination">
            <SelectValue placeholder="to" />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              <SelectItem value="*">* (any)</SelectItem>
              {services.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-1.5">
        <Input
          className="h-8 flex-1 font-mono text-xs"
          placeholder={suggested || 'policy name'}
          value={name}
          onChange={(e) => setName(e.target.value)}
          aria-label="Policy name"
          data-testid="policy-name"
        />
        <Button
          size="sm"
          onClick={apply}
          disabled={!from || !to || !effective}
          data-testid="policy-apply"
        >
          apply
        </Button>
      </div>
      {policies.length > 0 && (
        <ul className="flex flex-col gap-1">
          {policies.map((p) => (
            <li key={p.metadata.name} className="flex items-center gap-1.5 font-mono text-[11px]">
              <Badge
                variant="outline"
                className={
                  p.spec.action === 'DENY'
                    ? 'border-deny text-[9px] text-deny'
                    : 'border-traffic text-[9px] text-traffic'
                }
              >
                {p.spec.action}
              </Badge>
              <span className="flex-1 truncate">
                {p.metadata.name}{' '}
                <span className="text-muted-foreground">
                  {p.spec.rules.map((r) => `${r.from}→${r.to}`).join(', ')}
                </span>
              </span>
              <Button
                variant="ghost"
                size="icon-sm"
                className="size-5"
                aria-label={`Delete policy ${p.metadata.name}`}
                onClick={() => act(client.remove('NetworkPolicy', p.metadata.name))}
              >
                <Trash2Icon className="text-deny" />
              </Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
