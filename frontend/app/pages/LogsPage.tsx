import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import type { LogLine } from '@/api/types'
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { useClient } from '@/lib/client-context'
import { sortedInstances } from '@/store/selectors'
import { useSnapshot } from '@/store/store'

const RING_CAP = 500

// The mock stamps sim-seconds and the live stream stamps wall-clock epoch
// ms. The magnitude says which one arrived.
function stamp(ts: number): string {
  if (ts > 1e12) {
    return new Date(ts).toLocaleTimeString('en-GB', { hour12: false })
  }
  return `${(ts / 1000).toFixed(1)}s`
}

export default function LogsPage() {
  const client = useClient()
  const snapshot = useSnapshot()
  const navigate = useNavigate()
  const { instance } = useParams<{ instance: string }>()
  const [lines, setLines] = useState<LogLine[]>([])
  const [follow, setFollow] = useState(true)
  const [filter, setFilter] = useState('')
  const tailRef = useRef<HTMLDivElement>(null)

  const instances = sortedInstances(snapshot)
  const byWorkload = useMemo(() => {
    const groups = new Map<string, string[]>()
    for (const i of instances) {
      const list = groups.get(i.spec.workload) ?? []
      list.push(i.metadata.name)
      groups.set(i.spec.workload, list)
    }
    return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [instances])

  useEffect(() => {
    if (!instance) return
    setLines([])
    const unsubscribe = client.streamLogs(instance, (line) => {
      setLines((prev) => {
        const next = prev.length >= RING_CAP ? prev.slice(prev.length - RING_CAP + 1) : prev.slice()
        next.push(line)
        return next
      })
    })
    return unsubscribe
  }, [client, instance])

  // biome-ignore lint/correctness/useExhaustiveDependencies: `lines` is the trigger — new lines scroll the tail
  useEffect(() => {
    // scroll only the log pane, since scrollIntoView would also yank the window
    const pane = tailRef.current?.parentElement
    if (follow && pane) pane.scrollTop = pane.scrollHeight
  }, [lines, follow])

  const visible = filter ? lines.filter((l) => l.line.includes(filter)) : lines

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <Select value={instance ?? ''} onValueChange={(v) => navigate(`/logs/${v}`)}>
          <SelectTrigger className="w-56" aria-label="Instance to tail">
            <SelectValue placeholder="pick an instance" />
          </SelectTrigger>
          <SelectContent>
            {byWorkload.map(([wl, names]) => (
              <SelectGroup key={wl}>
                <SelectLabel className="font-mono">{wl}</SelectLabel>
                {names.map((n) => (
                  <SelectItem key={n} value={n} className="font-mono">
                    {n}
                  </SelectItem>
                ))}
              </SelectGroup>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="w-56"
          placeholder="filter lines"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          aria-label="Filter log lines"
        />
        <span className="flex items-center gap-2">
          <Switch id="follow" checked={follow} onCheckedChange={setFollow} />
          <Label htmlFor="follow">follow</Label>
        </span>
      </div>

      {!instance ? (
        <Empty className="postit-surface mx-auto mt-10 max-w-md p-6">
          <EmptyHeader>
            <EmptyTitle className="hand text-lg">no instance picked</EmptyTitle>
            <EmptyDescription>Choose one above to tail what that instance prints.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <div
          className="boardbox h-[65vh] overflow-y-auto p-4 font-mono text-xs leading-5"
          data-testid="log-pane"
        >
          {visible.map((l, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: the ring is append-only, so the index is the line identity
            <div key={`${l.ts}-${i}`} className="whitespace-pre-wrap">
              <span className="text-muted-foreground">{stamp(l.ts)} </span>
              <span className={l.line.includes('FAILED') ? 'text-deny' : undefined}>{l.line}</span>
            </div>
          ))}
          {visible.length === 0 && <span className="text-muted-foreground">nothing yet…</span>}
          <div ref={tailRef} />
        </div>
      )}
    </div>
  )
}
