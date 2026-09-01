// Add a whole new service (d, e, …) without writing YAML: a name and an
// instance count become a Workload + Service pair applied through the same
// channel everything else uses. The image stays fixed (a whoami that answers
// HTTP), and anyone who needs a different one has the apply page.

import { PlusIcon } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { act } from '@/lib/act'
import { useClient } from '@/lib/client-context'
import { newServiceYaml } from '@/lib/yamlgen'
import { sortedServices } from '@/store/selectors'
import { useSnapshot } from '@/store/store'

const DEFAULT_IMAGE = 'traefik/whoami:v1.10'

function nextName(taken: Set<string>): string {
  for (let i = 0; i < 26; i++) {
    const candidate = String.fromCharCode(97 + ((3 + i) % 26)) // start at d
    if (!taken.has(candidate)) return candidate
  }
  for (let i = 1; ; i++) {
    if (!taken.has(`svc-${i}`)) return `svc-${i}`
  }
}

export function AddServiceDialog() {
  const client = useClient()
  const snapshot = useSnapshot()
  const taken = new Set(sortedServices(snapshot).map((s) => s.metadata.name))
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [instances, setInstances] = useState('2')

  const effectiveName = name.trim() || nextName(taken)
  const parsedInstances = Number(instances)
  const valid =
    /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(effectiveName) &&
    !taken.has(effectiveName) &&
    Number.isInteger(parsedInstances) &&
    parsedInstances >= 0 &&
    parsedInstances <= 64

  const create = () => {
    if (!valid) return
    act(
      client.apply(newServiceYaml(effectiveName, DEFAULT_IMAGE, parsedInstances)).then((results) => {
        const failed = results.find((r) => r.action === 'error')
        if (failed) toast.error(failed.error ?? 'rejected')
        else {
          toast(`workload/${effectiveName} and service/${effectiveName} created`)
          setOpen(false)
          setName('')
        }
      }),
    )
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" data-testid="add-service">
          <PlusIcon data-icon="inline-start" />
          add service
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle className="font-mono">new service</DialogTitle>
          <DialogDescription>
            Creates a Workload and its Service through apply, running a whoami image that answers HTTP.
            The scheduler places it, every node answers with its own VIP, and it joins the traffic
            rotation on its own.
          </DialogDescription>
        </DialogHeader>
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="svc-name">name</FieldLabel>
            <Input
              id="svc-name"
              className="font-mono"
              placeholder={nextName(taken)}
              value={name}
              onChange={(e) => setName(e.target.value)}
              data-testid="new-service-name"
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="svc-instances">instances</FieldLabel>
            <Input
              id="svc-instances"
              className="font-mono"
              inputMode="numeric"
              value={instances}
              onChange={(e) => setInstances(e.target.value)}
            />
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button onClick={create} disabled={!valid} data-testid="new-service-create">
            create {effectiveName}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
