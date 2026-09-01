import { useEffect, useState } from 'react'
import { Navigate, NavLink, Route, Routes } from 'react-router-dom'
import { Toaster } from '@/components/ui/sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { useClientHandle } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import ApplyPage from '@/pages/ApplyPage'
import EtcdPage from '@/pages/EtcdPage'
import LogsPage from '@/pages/LogsPage'
import ResourcesPage from '@/pages/ResourcesPage'
import TopologyPage from '@/pages/TopologyPage'
import { useSnapshot } from '@/store/store'

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

// A segmented control picks the data source: mock runs the in-browser
// simulator, live connects to the real cluster through the facade, and the
// loser is torn down entirely (no simulation behind live). The dot reports
// the active source's health.
function ModeToggle() {
  const { client, setMode } = useClientHandle()
  const snap = useSnapshot()
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
  const live = client.mode === 'http'
  const segment = (label: 'mock' | 'live', active: boolean) => (
    <button
      key={label}
      type="button"
      role="radio"
      aria-checked={active}
      onClick={() => setMode(label === 'live' ? 'http' : 'mock')}
      data-testid={`mode-${label}`}
      className={cn(
        'cursor-pointer rounded-full px-2.5 py-0.5 uppercase tracking-wide transition-colors',
        active ? 'bg-ink text-card' : 'text-muted-foreground hover:text-foreground',
      )}
    >
      {label}
    </button>
  )
  return (
    <div
      role="radiogroup"
      aria-label="Data source"
      data-testid="mode-toggle"
      className="flex items-center gap-1 rounded-full border border-border bg-card p-0.5 pl-2.5 font-mono text-[11px]"
    >
      <span className={cn('size-2 rounded-full', ok && snap.synced ? 'bg-traffic' : 'bg-deny')} />
      {segment('mock', !live)}
      {segment('live', live)}
    </div>
  )
}

export default function App() {
  return (
    <TooltipProvider delayDuration={300}>
      <div className="min-h-screen">
        <header className="tray sticky top-0 z-40">
          <div className="mx-auto flex max-w-[1400px] items-center gap-6 px-5 py-3.5">
            <span className="font-mono text-2xl font-bold tracking-tight">k-lite</span>
            <nav className="flex flex-1 items-center gap-1 overflow-x-auto" aria-label="Pages">
              {TABS.map((t) => (
                <NavLink
                  key={t.to}
                  to={t.to}
                  end={t.to === '/'}
                  className={({ isActive }) =>
                    cn(
                      'rounded-full px-3.5 py-1.5 text-base whitespace-nowrap text-muted-foreground hover:text-foreground',
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
