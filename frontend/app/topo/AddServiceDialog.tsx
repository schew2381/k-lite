// Add a whole new service (d, e, …) without writing YAML: name, image, and
// replicas become a Workload + Service pair applied through the same
// channel everything else uses.

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
  const [image, setImage] = useState('traefik/whoami:v1.10')
  const [replicas, setReplicas] = useState('2')

  const effectiveName = name.trim() || nextName(taken)
  const parsedReplicas = Number(replicas)
  const valid =
    /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(effectiveName) &&
    !taken.has(effectiveName) &&
    Number.isInteger(parsedReplicas) &&
    parsedReplicas >= 0 &&
    parsedReplicas <= 64

  const create = () => {
    if (!valid) return
    act(
      client
        .apply(newServiceYaml(effectiveName, image.trim() || 'traefik/whoami:v1.10', parsedReplicas))
        .then((results) => {
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
            Creates a Workload and its Service through apply. The scheduler places it, every node answers
            with its own VIP, and it joins the traffic rotation on its own.
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
            <FieldLabel htmlFor="svc-image">image</FieldLabel>
            <Input
              id="svc-image"
              className="font-mono"
              value={image}
              onChange={(e) => setImage(e.target.value)}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="svc-replicas">replicas</FieldLabel>
            <Input
              id="svc-replicas"
              className="font-mono"
              inputMode="numeric"
              value={replicas}
              onChange={(e) => setReplicas(e.target.value)}
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
