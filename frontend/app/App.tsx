import { useEffect, useState } from 'react'
import { Navigate, NavLink, Route, Routes, useSearchParams } from 'react-router-dom'
import { Toaster } from '@/components/ui/sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { useClient } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import ApplyPage from '@/pages/ApplyPage'
import EtcdPage from '@/pages/EtcdPage'
import LogsPage from '@/pages/LogsPage'
import ResourcesPage from '@/pages/ResourcesPage'
import TopologyPage from '@/pages/TopologyPage'
import { useSnapshot } from '@/store/store'
import { useFlow } from '@/topo/flow'

const TABS = [
  { to: '/', label: 'topology' },
  { to: '/resources', label: 'resources' },
  { to: '/etcd', label: 'etcd' },
  { to: '/logs', label: 'logs' },
  { to: '/apply', label: 'apply' },
]

function Legend() {
  return (
    <div className="hidden items-center gap-4 text-xs text-muted-foreground lg:flex" aria-hidden>
      <span className="flex items-center gap-1.5">
        <span className="size-2.5 rounded-full bg-ctrl" /> control plane
      </span>
      <span className="flex items-center gap-1.5">
        <span className="size-2.5 rounded-full bg-traffic" /> real traffic
      </span>
      <span className="flex items-center gap-1.5">
        <span className="size-2.5 rounded-full bg-deny" /> policy
      </span>
    </div>
  )
}

// The pill is the one control: click it to flip between the mock walkthrough
// and live-speed traffic (ADR 0027). The dot still reports health.
function ModeToggle() {
  const client = useClient()
  const snap = useSnapshot()
  const flow = useFlow()
  const [searchParams, setSearchParams] = useSearchParams()
  const [ok, setOk] = useState(true)
  useEffect(() => {
    let alive = true
    const poll = () =>
      client
        .health()
        .then((h) => alive && setOk(h.ok))
        .catch(() => alive && setOk(false))
    poll()
    const t = setInterval(poll, 5000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [client])
  const live = flow === 'live'
  const flip = () => {
    const next = new URLSearchParams(searchParams)
    next.set('flow', live ? 'traced' : 'live')
    setSearchParams(next, { replace: true })
  }
  return (
    <button
      type="button"
      onClick={flip}
      aria-pressed={live}
      aria-label={live ? 'Switch to the mock walkthrough' : 'Switch to live-speed traffic'}
      data-testid="mode-toggle"
      className={cn(
        'flex cursor-pointer items-center gap-1.5 rounded-full border px-2.5 py-0.5 font-mono text-[11px] uppercase tracking-wide transition-colors',
        live ? 'border-ctrl bg-accent text-ctrl' : 'border-border bg-card',
      )}
    >
      <span
        className={cn(
          'size-2 rounded-full',
          ok && snap.synced ? (live ? 'bg-ctrl' : 'bg-traffic') : 'bg-deny',
        )}
      />
      {live ? 'live' : 'mock'}
    </button>
  )
}

export default function App() {
  return (
    <TooltipProvider delayDuration={300}>
      <div className="min-h-screen">
        <header className="tray sticky top-0 z-40">
          <div className="mx-auto flex max-w-[1400px] items-center gap-5 px-5 py-2">
            <span className="hand text-xl font-bold tracking-wide">k-lite</span>
            <nav className="flex flex-1 items-center gap-1 overflow-x-auto" aria-label="Pages">
              {TABS.map((t) => (
                <NavLink
                  key={t.to}
                  to={t.to}
                  end={t.to === '/'}
                  className={({ isActive }) =>
                    cn(
                      'rounded-full px-3 py-1 text-sm whitespace-nowrap text-muted-foreground hover:text-foreground',
                      isActive && 'border border-[#a8a7a0] bg-white/55 text-foreground',
                    )
                  }
                >
                  {t.label}
                </NavLink>
              ))}
            </nav>
            <Legend />
            <ModeToggle />
          </div>
        </header>
        <main className="mx-auto max-w-[1400px] px-5 py-6">
          <Routes>
            <Route path="/" element={<TopologyPage />} />
            <Route path="/resources" element={<ResourcesPage />} />
            <Route path="/etcd" element={<EtcdPage />} />
            <Route path="/logs/:instance?" element={<LogsPage />} />
            <Route path="/apply" element={<ApplyPage />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </main>
        <Toaster position="bottom-right" />
      </div>
    </TooltipProvider>
  )
}
