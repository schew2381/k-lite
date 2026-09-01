import { useEffect, useState } from 'react'
import { Navigate, NavLink, Route, Routes, useSearchParams } from 'react-router-dom'
import { Toaster } from '@/components/ui/sonner'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
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

// Flip between the walkthrough and real-speed traffic (ADR 0027). The URL
// carries the choice, so a link reproduces what the toggle set.
function FlowToggle() {
  const flow = useFlow()
  const [searchParams, setSearchParams] = useSearchParams()
  const set = (v: string) => {
    if (v !== 'live' && v !== 'traced') return
    const next = new URLSearchParams(searchParams)
    next.set('flow', v)
    setSearchParams(next, { replace: true })
  }
  return (
    <ToggleGroup
      type="single"
      size="sm"
      variant="outline"
      value={flow}
      onValueChange={set}
      aria-label="Traffic flow"
      data-testid="flow-toggle"
    >
      <ToggleGroupItem value="traced" aria-label="Traced flow">
        traced
      </ToggleGroupItem>
      <ToggleGroupItem value="live" aria-label="Live flow">
        live
      </ToggleGroupItem>
    </ToggleGroup>
  )
}

function SpeedControl() {
  const client = useClient()
  const [speed, setSpeed] = useState(client.chaos?.speed() ?? 1)
  if (!client.chaos) return null
  const set = (v: string) => {
    if (!v) return
    const x = Number(v)
    client.chaos?.setSpeed(x)
    setSpeed(x)
  }
  return (
    <ToggleGroup
      type="single"
      size="sm"
      variant="outline"
      value={String(speed)}
      onValueChange={set}
      aria-label="Simulation speed"
    >
      <ToggleGroupItem value="0" aria-label="Pause">
        ⏸
      </ToggleGroupItem>
      <ToggleGroupItem value="1" aria-label="Normal speed">
        1×
      </ToggleGroupItem>
      <ToggleGroupItem value="4" aria-label="Four times speed">
        4×
      </ToggleGroupItem>
    </ToggleGroup>
  )
}

function HealthPill() {
  const client = useClient()
  const snap = useSnapshot()
  const [ok, setOk] = useState(true)
  useEffect(() => {
    let live = true
    const poll = () =>
      client
        .health()
        .then((h) => live && setOk(h.ok))
        .catch(() => live && setOk(false))
    poll()
    const t = setInterval(poll, 5000)
    return () => {
      live = false
      clearInterval(t)
    }
  }, [client])
  return (
    <span className="flex items-center gap-1.5 rounded-full border border-border bg-card px-2.5 py-0.5 font-mono text-[11px] uppercase tracking-wide">
      <span className={cn('size-2 rounded-full', ok && snap.synced ? 'bg-traffic' : 'bg-deny')} />
      {client.mode === 'http' ? 'live' : 'mock'}
    </span>
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
            <FlowToggle />
            <SpeedControl />
            <HealthPill />
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
