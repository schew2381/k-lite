// Against a live cluster, applying a Node object only declares membership
// (ADR 0018): a machine still has to join by running klite-agent with a
// token. This dialog hands over the exact command for either home — the
// machine already running klited, or a fresh one across the internet.

import { CheckIcon, CopyIcon } from 'lucide-react'
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

export interface JoinInfo {
  node: string
  token: string
  endpoints: string[]
}

// A remote machine can't dial 127.0.0.1, so the internet command needs the
// cluster's routable address. The browser's own hostname is the best guess
// when the UI isn't being viewed on the cluster machine itself.
function routableServer(endpoints: string[]): string {
  const port = endpoints[0]?.split(':')[1] ?? '7443'
  const host = typeof window !== 'undefined' ? window.location.hostname : ''
  if (host && host !== 'localhost' && host !== '127.0.0.1') return `${host}:${port}`
  return `<machine-address>:${port}`
}

export function joinCommand(info: JoinInfo, where: 'local' | 'internet'): string {
  if (where === 'local') {
    // --server defaults to 127.0.0.1:7443, which is exactly where klited is
    return `klite-agent --node ${info.node} --token ${info.token}`
  }
  return `klite-agent --node ${info.node} --server ${routableServer(info.endpoints)} --token ${info.token}`
}

function CommandBlock({ command }: { command: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <>
      <pre className="overflow-x-auto rounded-lg border border-board-edge bg-card p-3 font-mono text-[11.5px] leading-5">
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
            {info.node} is declared and waits for its machine. Pick where it should live — the token is
            single-use and the agent does the rest.
          </DialogDescription>
        </DialogHeader>
        <Tabs defaultValue="local">
          <TabsList className="w-full font-mono">
            <TabsTrigger value="local" className="flex-1 text-xs" data-testid="join-local">
              this machine
            </TabsTrigger>
            <TabsTrigger value="internet" className="flex-1 text-xs" data-testid="join-internet">
              over the internet
            </TabsTrigger>
          </TabsList>
          <TabsContent value="local" className="flex flex-col gap-2">
            <CommandBlock command={joinCommand(info, 'local')} />
            <p className="text-xs text-muted-foreground">
              Run it where klited already runs. The default server address points at it.
            </p>
          </TabsContent>
          <TabsContent value="internet" className="flex flex-col gap-2">
            <CommandBlock command={joinCommand(info, 'internet')} />
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
