// Against a live cluster, applying a Node object only declares membership
// (ADR 0018): a machine still has to join by running klite-agent with a
// token. This dialog covers both homes. On this machine, one click asks the
// facade to start the agent (ADR 0040). Across the internet, it hands over
// the exact command, since no facade route can reach a machine it isn't on.

import { CheckIcon, CopyIcon, Loader2Icon } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useClient } from '@/lib/client-context'

export interface JoinInfo {
  node: string
  token: string
  endpoints: string[]
}

// A remote machine can't dial 127.0.0.1, so the internet command needs the
// cluster's routable address. The browser's own hostname is the best guess
// when the browser itself isn't on the cluster machine.
function routableServer(endpoints: string[]): string {
  const port = endpoints[0]?.split(':')[1] ?? '7443'
  const host = typeof window !== 'undefined' ? window.location.hostname : ''
  if (host && host !== 'localhost' && host !== '127.0.0.1') return `${host}:${port}`
  return `<machine-address>:${port}`
}

export function internetJoinCommand(info: JoinInfo): string {
  return `klite-agent --node ${info.node} --server ${routableServer(info.endpoints)} --token ${info.token}`
}

function CommandBlock({ command }: { command: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <>
      <pre className="whitespace-pre-wrap break-all rounded-lg border border-board-edge bg-card p-3 font-mono text-[11.5px] leading-5">
        {command}
      </pre>
      <Button
        size="sm"
        variant="outline"
        data-testid="copy-join-command"
        onClick={() => {
          navigator.clipboard.writeText(command).then(() => {
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
          })
        }}
      >
        {copied ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
        {copied ? 'copied' : 'copy command'}
      </Button>
    </>
  )
}

// LocalJoin is the one-click path. It falls back to the copyable command
// when the client can't spawn agents (a facade started without -agent-bin
// says so itself).
function LocalJoin({ info, onDone }: { info: JoinInfo; onDone: () => void }) {
  const client = useClient()
  const [state, setState] = useState<'idle' | 'starting' | 'done'>('idle')
  const [error, setError] = useState<string | null>(null)

  if (!client.joinNode) {
    return <CommandBlock command={`klite-agent --node ${info.node} --token ${info.token}`} />
  }

  async function joinNow() {
    if (!client.joinNode) return
    setState('starting')
    setError(null)
    try {
      await client.joinNode(info.node)
      setState('done')
      setTimeout(onDone, 1200)
    } catch (e) {
      setState('idle')
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <>
      <Button data-testid="join-now" disabled={state !== 'idle'} onClick={joinNow}>
        {state === 'starting' && <Loader2Icon className="animate-spin" data-icon="inline-start" />}
        {state === 'done' && <CheckIcon data-icon="inline-start" />}
        {state === 'done'
          ? 'agent started'
          : state === 'starting'
            ? 'starting…'
            : `join ${info.node} now`}
      </Button>
      {error ? (
        <p className="text-xs text-policy">{error}</p>
      ) : (
        <p className="text-xs text-muted-foreground">
          {state === 'done'
            ? 'Watch its card: the node goes Ready once the agent registers.'
            : 'Starts klite-agent here, on the machine already running klited.'}
        </p>
      )}
    </>
  )
}

export function JoinNodeDialog({
  info,
  onOpenChange,
}: {
  info: JoinInfo | null
  onOpenChange: (open: boolean) => void
}) {
  if (!info) return null

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="font-mono">join {info.node}</DialogTitle>
          <DialogDescription>
            {info.node} is declared and waits for its machine. Pick where it should live.
          </DialogDescription>
        </DialogHeader>
        <Tabs defaultValue="local" className="min-w-0">
          <TabsList className="w-full font-mono">
            <TabsTrigger value="local" className="flex-1 text-xs" data-testid="join-local">
              this machine
            </TabsTrigger>
            <TabsTrigger value="internet" className="flex-1 text-xs" data-testid="join-internet">
              over the internet
            </TabsTrigger>
          </TabsList>
          <TabsContent value="local" className="flex flex-col gap-2">
            <LocalJoin info={info} onDone={() => onOpenChange(false)} />
          </TabsContent>
          <TabsContent value="internet" className="flex flex-col gap-2">
            <CommandBlock command={internetJoinCommand(info)} />
            <p className="text-xs text-muted-foreground">
              Run it on the new machine. It needs to reach the cluster address above, and the cluster
              needs to reach it back for the mTLS ingress (ADR 0034).
            </p>
          </TabsContent>
        </Tabs>
      </DialogContent>
    </Dialog>
  )
}
