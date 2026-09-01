import { Navigate, NavLink, Route, Routes, useSearchParams } from 'react-router-dom'
import { Toaster } from '@/components/ui/sonner'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { TooltipProvider } from '@/components/ui/tooltip'
import { useClientHandle } from '@/lib/client-context'
import { cn } from '@/lib/utils'
import ApplyPage from '@/pages/ApplyPage'
import EtcdPage from '@/pages/EtcdPage'
import LogsPage from '@/pages/LogsPage'
import ResourcesPage from '@/pages/ResourcesPage'
import TopologyPage from '@/pages/TopologyPage'

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
// loser is torn down entirely (no simulation behind live).
function ModeToggle() {
  const { client, setMode } = useClientHandle()
  const [searchParams, setSearchParams] = useSearchParams()
  const live = client.mode === 'http'
  const pick = (v: string) => {
    setMode(v === 'live' ? 'http' : 'mock')
    // the URL carries the source, so a shared link opens in the same mode
    const next = new URLSearchParams(searchParams)
    next.set('mode', v === 'live' ? 'live' : 'mock')
    setSearchParams(next, { replace: true })
  }
  return (
    <Tabs value={live ? 'live' : 'mock'} onValueChange={pick}>
      <TabsList aria-label="Data source" data-testid="mode-toggle" className="rounded-full font-mono">
        <TabsTrigger
          value="mock"
          data-testid="mode-mock"
          className="cursor-pointer rounded-full px-3 text-[11px] uppercase tracking-wide transition-all duration-200"
        >
          mock
        </TabsTrigger>
        <TabsTrigger
          value="live"
          data-testid="mode-live"
          className="cursor-pointer rounded-full px-3 text-[11px] uppercase tracking-wide transition-all duration-200"
        >
          live
        </TabsTrigger>
      </TabsList>
    </Tabs>
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
