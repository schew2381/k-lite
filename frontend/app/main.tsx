import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { HashRouter } from 'react-router-dom'
import { createClient, createClientFor } from '@/api/client'
import { ClientProvider } from '@/lib/client-context'
import App from './App'
import './index.css'

// A fresh link can open straight into either source: ?mode=live|mock wins,
// then the build-time default. HashRouter keeps the query inside the hash.
const urlMode = new URLSearchParams(window.location.hash.split('?')[1]).get('mode')
const client = await (urlMode === 'live'
  ? createClientFor('http')
  : urlMode === 'mock'
    ? createClientFor('mock')
    : createClient())

createRoot(document.getElementById('root') as HTMLElement).render(
  <StrictMode>
    <ClientProvider client={client}>
      <HashRouter>
        <App />
      </HashRouter>
    </ClientProvider>
  </StrictMode>,
)
