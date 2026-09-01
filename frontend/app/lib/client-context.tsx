import { createContext, useContext } from 'react'
import type { KliteClient } from '@/api/client'

const ClientContext = createContext<KliteClient | null>(null)

export function ClientProvider({
  client,
  children,
}: {
  client: KliteClient
  children: React.ReactNode
}) {
  return <ClientContext.Provider value={client}>{children}</ClientContext.Provider>
}

export function useClient(): KliteClient {
  const client = useContext(ClientContext)
  if (!client) throw new Error('useClient outside <ClientProvider>')
  return client
}
