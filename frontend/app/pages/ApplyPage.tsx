import { CheckIcon, ChevronDownIcon, XIcon } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import type { ApplyResult } from '@/api/types'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Textarea } from '@/components/ui/textarea'
import { useClient } from '@/lib/client-context'
import { EXAMPLES } from '@/lib/examples'
import { cn } from '@/lib/utils'

export default function ApplyPage() {
  const client = useClient()
  const [text, setText] = useState('')
  const [results, setResults] = useState<ApplyResult[] | null>(null)
  const [busy, setBusy] = useState(false)

  const apply = async () => {
    setBusy(true)
    try {
      setResults(await client.apply(text))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'apply failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_320px]">
      <div className="flex flex-col gap-3">
        <div className="flex items-center justify-between">
          <p className="text-sm text-muted-foreground">
            Declare what should exist: multi-document YAML, exactly what{' '}
            <code className="font-mono">klite apply</code> takes.
          </p>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="outline" size="sm">
                insert example
                <ChevronDownIcon data-icon="inline-end" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuGroup>
                {EXAMPLES.map((ex) => (
                  <DropdownMenuItem
                    key={ex.label}
                    onClick={() =>
                      setText((t) => (t.trim() ? `${t.trimEnd()}\n---\n${ex.yaml}` : ex.yaml))
                    }
                  >
                    {ex.label}
                  </DropdownMenuItem>
                ))}
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        <Textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={'apiVersion: klite/v1\nkind: NetworkPolicy\n…'}
          spellCheck={false}
          className="min-h-[55vh] rounded-lg border-[1.5px] border-ink bg-card font-mono text-[13px] shadow-[2px_3px_0_rgba(43,42,36,0.12)]"
          aria-label="YAML to apply"
          data-testid="apply-input"
        />
        <div>
          <Button onClick={apply} disabled={busy || text.trim() === ''} data-testid="apply-button">
            apply
          </Button>
        </div>
      </div>

      <aside className="flex flex-col gap-2">
        <h2 className="eyebrow">results</h2>
        {results === null ? (
          <p className="text-sm text-muted-foreground">Nothing applied yet this visit.</p>
        ) : (
          <ul className="flex flex-col gap-1.5" data-testid="apply-results">
            {results.map((r, i) => (
              <li
                // biome-ignore lint/suspicious/noArrayIndexKey: results align 1:1 with the applied documents
                key={`${r.kind}-${r.name}-${i}`}
                className={cn(
                  'flex items-start gap-2 rounded-lg border-[1.5px] px-3 py-2 text-sm',
                  r.action === 'error' ? 'border-deny bg-deny/10' : 'border-traffic bg-traffic/10',
                )}
              >
                {r.action === 'error' ? (
                  <XIcon className="mt-0.5 size-4 shrink-0 text-deny" />
                ) : (
                  <CheckIcon className="mt-0.5 size-4 shrink-0 text-traffic" />
                )}
                <span>
                  <span className="font-mono font-semibold">
                    {r.kind.toLowerCase()}/{r.name}
                  </span>{' '}
                  {r.action}
                  {r.error && <span className="block text-xs text-deny">{r.error}</span>}
                </span>
              </li>
            ))}
          </ul>
        )}
      </aside>
    </div>
  )
}
