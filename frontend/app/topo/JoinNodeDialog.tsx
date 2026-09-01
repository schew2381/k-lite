// Against a live cluster, applying a Node object only declares membership
// (ADR 0018): a machine still has to join by running klite-agent with a
// token. This dialog hands over the exact command, token included.

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

export interface JoinInfo {
  node: string
  token: string
  endpoints: string[]
}

export function joinCommand(info: JoinInfo): string {
  const server = info.endpoints.join(',') || '127.0.0.1:7443'
  return `klite-agent --node ${info.node} --server ${server} --token ${info.token}`
}

export function JoinNodeDialog({
  info,
  onOpenChange,
}: {
  info: JoinInfo | null
  onOpenChange: (open: boolean) => void
}) {
  const [copied, setCopied] = useState(false)
  if (!info) return null
  const command = joinCommand(info)

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="font-mono">join {info.node}</DialogTitle>
          <DialogDescription>
            {info.node} is declared and waits for its machine. Run this on the machine that should become{' '}
            {info.node} — the token is single-use and the agent does the rest.
          </DialogDescription>
        </DialogHeader>
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
      </DialogContent>
    </Dialog>
  )
}
