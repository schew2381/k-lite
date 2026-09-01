import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { HashRouter } from 'react-router-dom'
import { createClient } from '@/api/client'
import { ClientProvider } from '@/lib/client-context'
import App from './App'
import './index.css'

const client = await createClient()

createRoot(document.getElementById('root') as HTMLElement).render(
  <StrictMode>
    <ClientProvider client={client}>
      <HashRouter>
        <App />
      </HashRouter>
    </ClientProvider>
  </StrictMode>,
)
