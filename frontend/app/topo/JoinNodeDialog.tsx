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
  machineAddresses?: string[]
}

// A remote machine can't dial 127.0.0.1, so the internet command needs the
// cluster's routable address. The browser's own hostname is the best guess
// when the browser itself isn't on the cluster machine. When it is, the
// facade's own interface addresses stand in.
function routableServer(info: JoinInfo): string {
  const port = info.endpoints[0]?.split(':')[1] ?? '7443'
  const host = typeof window !== 'undefined' ? window.location.hostname : ''
  if (host && host !== 'localhost' && host !== '127.0.0.1') return `${host}:${port}`
  const lan = info.machineAddresses?.[0]
  return lan ? `${lan}:${port}` : `<machine-address>:${port}`
}

// The advertise address stays a placeholder on purpose: it names the NEW
// machine, which only that machine knows. Without it, a Linux box guesses
// its Docker bridge and every node dials into nothing (ADR 0034).
export function internetJoinCommand(info: JoinInfo): string {
  return `klite-agent --node ${info.node} --server ${routableServer(info)} --token ${info.token} --advertise-address <new-machine-address>`
}

// A tailnet address means the joining machine must be on the tailnet too,
// which join.sh handles during install (KLITE_VPN=tailscale). 100.64/10 is
// the CGNAT space tailnets hand out.
function isTailnetAddress(host: string): boolean {
  const m = host.match(/^100\.(\d+)\./)
  return m !== null && Number(m[1]) >= 64 && Number(m[1]) <= 127
}

// The same one-liner klite node add prints: join.sh installs what a fresh
// Linux box is missing and runs the agent under systemd (ADR 0038).
export function oneLinerJoinCommand(info: JoinInfo): string {
  const server = routableServer(info)
  const env = `KLITE_URL=${server} KLITE_TOKEN='${info.token}' KLITE_NODE=${info.node}`
  const vpn = isTailnetAddress(server.split(':')[0])
    ? " KLITE_VPN=tailscale KLITE_TS_AUTHKEY='tskey-auth-…' KLITE_YES=1"
    : ''
  return `curl -sfL https://github.com/schew2381/k-lite/releases/latest/download/join.sh | ${env}${vpn} sh -`
}

function CommandBlock({ command, testid = 'copy-join-command' }: { command: string; testid?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <>
      <pre className="whitespace-pre-wrap break-all rounded-lg border border-board-edge bg-card p-3 font-mono text-[11.5px] leading-5">
        {command}
      </pre>
      <Button
        size="sm"
        variant="outline"
        data-testid={testid}
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
            : 'Starts klite-agent on the machine already running klited.'}
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
            <p className="eyebrow text-[10px]">linux, one line</p>
            <CommandBlock command={oneLinerJoinCommand(info)} testid="copy-join-oneliner" />
            <p className="eyebrow text-[10px]">by hand, any OS</p>
            <CommandBlock command={internetJoinCommand(info)} testid="copy-join-command" />
            <p className="text-xs text-muted-foreground">
              Run either on the new machine, filling in that machine's own routable address. Other nodes
              dial that address for the mTLS ingress, so each side has to reach the other (ADR 0034).
            </p>
          </TabsContent>
        </Tabs>
      </DialogContent>
    </Dialog>
  )
}
