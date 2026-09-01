import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { HashRouter } from 'react-router-dom'
import { createClient } from '@/api/client'
import { ClientProvider } from '@/lib/client-context'
import { clusterStore } from '@/store/store'
import { trafficLog } from '@/store/trafficLog'
import App from './App'
import './index.css'

const client = await createClient()
client.watch(clusterStore.applyEvent)
client.watchTraffic(trafficLog.push)

createRoot(document.getElementById('root') as HTMLElement).render(
  <StrictMode>
    <ClientProvider client={client}>
      <HashRouter>
        <App />
      </HashRouter>
    </ClientProvider>
  </StrictMode>,
)
