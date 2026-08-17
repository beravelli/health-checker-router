import { useCallback, useState } from 'react'
import { Card, ErrorNote, Loading, relativeTime } from './components/ui'
import { api } from './lib/api'
import { useAsync, useEventStream, useLocalState } from './lib/hooks'
import { Admin, AccessDenied } from './tabs/Admin'
import { Failover } from './tabs/Failover'
import { Incidents } from './tabs/Incidents'
import { Overview } from './tabs/Overview'
import { Rollouts } from './tabs/Rollouts'
import { Services } from './tabs/Services'

type TabId = 'overview' | 'services' | 'failover' | 'rollouts' | 'incidents' | 'admin'

const TABS: { id: TabId; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'services', label: 'Services' },
  { id: 'failover', label: 'Failover & routing' },
  { id: 'rollouts', label: 'Rollouts' },
  { id: 'incidents', label: 'Incidents' },
  { id: 'admin', label: 'Admin' },
]

export default function App() {
  const [tab, setTab] = useLocalState<TabId>('hcr.tab', 'overview')
  const [severity, setSeverity] = useLocalState<string>('hcr.severity', 'all')
  const [bannerDismissed, setBannerDismissed] = useState(false)

  const me = useAsync(() => api.me(), [])

  // One combined load keeps the tabs consistent with each other: a health
  // snapshot paired with weights from a different moment would show a traffic
  // split that never actually existed.
  const data = useAsync(
    () =>
      Promise.all([
        api.health(),
        api.regions(),
        api.services(),
        api.rollouts(),
        api.weights(),
        api.failOpen(),
        api.routingEndpoints(),
      ]),
    [],
  )

  const incidents = useAsync(() => api.incidents(severity), [severity])

  const reloadAll = useCallback(() => {
    void data.reload()
    void incidents.reload()
  }, [data, incidents])

  // Live updates. Any event just triggers a refetch rather than patching local
  // state — the payloads are small and this keeps a single source of truth.
  const connected = useEventStream(() => {
    void data.reload()
  }, me.data?.authenticated ?? false)

  if (me.loading) return <Loading label="Signing in" />

  if (me.data && !me.data.authenticated) {
    return <LoginScreen onSignedIn={() => void me.reload()} />
  }

  if (data.error) {
    return (
      <div className="shell">
        <ErrorNote error={data.error} onRetry={() => void data.reload()} />
      </div>
    )
  }

  if (!data.data) return <Loading label="Loading health data" />

  const [snapshot, regionsRaw, servicesRaw, rolloutsRaw, weights, failOpen, routing] = data.data
  const regions = regionsRaw ?? []
  const services = servicesRaw ?? []
  const rollouts = rolloutsRaw ?? []

  const isAdmin = me.data?.isAdmin ?? false
  const adminGroup = me.data?.adminGroup ?? 'sre-admins'
  const showBanner = failOpen.active && !bannerDismissed

  return (
    <div className="page">
      <nav className="nav">
        <span className="nav-brand">Meridian Health Watch</span>
        {TABS.map((t) => (
          <a
            key={t.id}
            href="#"
            aria-current={tab === t.id ? 'page' : undefined}
            onClick={(e) => {
              e.preventDefault()
              setTab(t.id)
            }}
          >
            {t.label}
          </a>
        ))}
        <div className="nav-right">
          <span
            className="tag tag-neutral"
            title={connected ? 'Receiving live updates' : 'Live stream disconnected — reconnecting'}
            style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}
          >
            <span className={`live-dot${connected ? '' : ' stale'}`} />
            {connected ? 'Live' : 'Reconnecting'}
          </span>
          <span className="tag tag-outline">{regions.map((r) => r.displayName).join(' & ')}</span>
          <span className="tag tag-neutral" title={(me.data?.user?.groups ?? []).join(', ')}>
            {me.data?.user?.username ?? 'unknown'}
            {isAdmin ? ' · admin' : ''}
          </span>
          <button
            className="btn btn-ghost"
            onClick={() => {
              void api.logout().then(() => me.reload())
            }}
          >
            Sign out
          </button>
        </div>
      </nav>

      {showBanner ? (
        <div className="banner">
          <span
            className="tag"
            style={{ background: 'var(--color-accent-500)', color: 'var(--color-bg)' }}
          >
            FAIL-OPEN ACTIVE
          </span>
          <span>
            {failOpen.event?.reason ??
              'Health checks are inconclusive — LB is routing on last-known-good state per fail-open policy.'}{' '}
            Verify before trusting these health signals.
            {failOpen.event ? ` Active since ${relativeTime(failOpen.event.triggeredAt)}.` : ''}
          </span>
          <button className="btn btn-ghost" style={{ marginLeft: 'auto' }} onClick={() => setBannerDismissed(true)}>
            Dismiss
          </button>
        </div>
      ) : null}

      <div className="shell">
        {tab === 'overview' ? <Overview snapshot={snapshot} /> : null}
        {tab === 'services' ? <Services snapshot={snapshot} regions={regions} /> : null}
        {tab === 'failover' ? (
          <Failover
            regions={regions}
            weights={weights}
            failOpen={failOpen}
            routing={routing}
            isAdmin={isAdmin}
            onChanged={reloadAll}
          />
        ) : null}
        {tab === 'rollouts' ? (
          <Rollouts rollouts={rollouts} isAdmin={isAdmin} onChanged={reloadAll} />
        ) : null}
        {tab === 'incidents' ? (
          <Incidents
            incidents={incidents.data ?? []}
            filter={severity}
            onFilterChange={setSeverity}
          />
        ) : null}
        {tab === 'admin' ? (
          isAdmin ? (
            <Admin
              regions={regions}
              services={services}
              routing={routing}
              onChanged={reloadAll}
            />
          ) : (
            <AccessDenied group={adminGroup} currentGroups={me.data?.user?.groups ?? []} />
          )
        ) : null}
      </div>
    </div>
  )
}

function LoginScreen({ onSignedIn }: { onSignedIn: () => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.login(username, password)
      onSignedIn()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Sign in failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: 24,
      }}
    >
      <Card elevation="lg" style={{ maxWidth: 400, width: '100%' }}>
        <div className="card-kicker">Meridian Health Watch</div>
        <h3 style={{ marginBottom: 16 }}>Sign in</h3>
        <form onSubmit={submit}>
          <div className="field">
            <label htmlFor="username">Username</label>
            <input
              id="username"
              type="text"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="password">Password</label>
            <input
              id="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          {error ? (
            <p style={{ color: 'var(--color-critical-fg)', fontSize: 13 }}>{error}</p>
          ) : null}
          <button className="btn btn-primary" type="submit" disabled={busy} style={{ width: '100%' }}>
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </form>
        <p className="text-muted" style={{ fontSize: 12, marginTop: 14, marginBottom: 0 }}>
          Authenticated against your LDAP directory. Admin actions additionally require membership
          in the configured admin group.
        </p>
      </Card>
    </div>
  )
}
