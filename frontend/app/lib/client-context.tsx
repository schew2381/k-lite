// The provider owns the active client: it wires the watch and traffic
// streams into the stores, and swapping modes tears the old client all the
// way down (the mock's clock included) before the new one takes over, so
// live mode never has a simulation running underneath it.

import { createContext, useContext, useEffect, useRef, useState } from 'react'
import { createClientFor, type KliteClient } from '@/api/client'
import { clusterStore } from '@/store/store'
import { traceStore } from '@/store/traceStore'
import { trafficLog } from '@/store/trafficLog'

interface ClientHandle {
  client: KliteClient
  setMode: (mode: 'mock' | 'http') => void
}

const ClientContext = createContext<ClientHandle | null>(null)

export function ClientProvider({
  client: initial,
  children,
}: {
  client: KliteClient
  children: React.ReactNode
}) {
  const [client, setClient] = useState(initial)
  const swapping = useRef(false)

  // stream wiring follows the active client. Cleanup only unsubscribes:
  // disposal belongs to the swap, or StrictMode's probe mount would stop the
  // initial mock's clock for good.
  useEffect(() => {
    const unwatch = client.watch(clusterStore.applyEvent)
    const untraffic = client.watchTraffic(trafficLog.push)
    return () => {
      unwatch()
      untraffic()
    }
  }, [client])

  const setMode = (mode: 'mock' | 'http') => {
    if (mode === client.mode || swapping.current) return
    swapping.current = true
    void createClientFor(mode)
      .then((next) => {
        client.dispose?.()
        clusterStore.applyEvent({ type: 'RESET', rev: 0 })
        trafficLog.clear()
        traceStore.set(null)
        setClient(next)
      })
      .finally(() => {
        swapping.current = false
      })
  }

  return <ClientContext.Provider value={{ client, setMode }}>{children}</ClientContext.Provider>
}

export function useClientHandle(): ClientHandle {
  const handle = useContext(ClientContext)
  if (!handle) throw new Error('useClientHandle outside <ClientProvider>')
  return handle
}

export function useClient(): KliteClient {
  return useClientHandle().client
}
