import { useState } from 'react'
import { Card, Dialog, EmptyNote, StatusTag, relativeTime } from '../components/ui'
import type { HealthSnapshot, RegionHealth } from '../lib/types'

/** Bar colour follows the same health thresholds as the status tags. */
function barColor(uptime: number): string {
  if (uptime >= 99) return 'var(--color-accent-2-500)'
  if (uptime >= 97) return 'var(--color-accent-500)'
  return 'var(--color-critical-base)'
}

/**
 * Bars are scaled across 90–100% rather than 0–100%. Uptime almost never drops
 * below 90, so a zero-based axis would render every bar identical.
 */
function barHeight(uptime: number): string {
  return `${Math.max(6, (uptime - 90) * 5)}px`
}

function RegionCard({ region, onOpen }: { region: RegionHealth; onOpen: () => void }) {
  return (
    <Card elevation="md" onClick={onOpen}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <div>
          <div className="card-kicker">{region.zone || 'region'}</div>
          <div className="card-title" style={{ fontSize: 24 }}>
            {region.name}
          </div>
        </div>
        <StatusTag status={region.status} size="lg" />
      </div>

      <div className="grid-4" style={{ margin: '18px 0 6px' }}>
        <div>
          <div className="metric-value">{region.trafficPct}%</div>
          <div className="metric-label">Traffic share</div>
        </div>
        <div>
          <div className="metric-value">{region.latencyMs}ms</div>
          <div className="metric-label">p99 latency</div>
        </div>
        <div>
          <div className="metric-value">{region.errorRatePct}%</div>
          <div className="metric-label">Error rate</div>
        </div>
        <div>
          <div className="metric-value">{region.replicationLagMs}ms</div>
          <div className="metric-label">Repl. lag</div>
        </div>
      </div>

      <div className="hr" style={{ margin: '8px 0' }} />

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
        {(region.deps ?? []).length > 0 ? (
          (region.deps ?? []).map((d) => (
            <span key={d.name} className={`tag status-${d.status}`}>
              {d.name}
            </span>
          ))
        ) : (
          <span className="text-muted" style={{ fontSize: 13 }}>
            No dependency checks reporting yet
          </span>
        )}
      </div>

      <div className="card-meta">
        <span>version {region.version || '—'}</span>
        <span>·</span>
        <span>uptime {region.uptimePct}% (30d)</span>
      </div>
    </Card>
  )
}

export function Overview({ snapshot }: { snapshot: HealthSnapshot }) {
  const [selected, setSelected] = useState<RegionHealth | null>(null)
  const regions = snapshot.regions ?? []

  if (regions.length === 0) {
    return (
      <>
        <h1 style={{ marginBottom: 2 }}>Overview</h1>
        <p className="text-muted" style={{ marginBottom: 24 }}>
          Live health across both active-active sites.
        </p>
        <EmptyNote>
          No regions are configured yet. Add them in <strong>Admin</strong> to start collecting
          health data.
        </EmptyNote>
      </>
    )
  }

  return (
    <>
      <h1 style={{ marginBottom: 2 }}>Overview</h1>
      <p className="text-muted" style={{ marginBottom: 24 }}>
        Live health across both active-active sites. Both regions serve production traffic
        concurrently.
      </p>

      <div className="grid-2">
        {regions.map((r) => (
          <RegionCard key={r.regionId} region={r} onOpen={() => setSelected(r)} />
        ))}
      </div>

      <h2 style={{ marginTop: 36 }}>7-day uptime trend</h2>
      <Card elevation="sm" style={{ padding: 20 }}>
        {regions.map((r) => {
          const trend = r.trend ?? []
          return (
            <div key={r.regionId} style={{ marginBottom: 16 }}>
              <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>{r.name}</div>
              {trend.length === 0 ? (
                <div className="text-muted" style={{ fontSize: 13 }}>
                  Not enough probe history yet — the trend fills in as data accumulates.
                </div>
              ) : (
                <div style={{ display: 'flex', alignItems: 'flex-end', gap: 8, height: 60 }}>
                  {trend.map((pt, i) => (
                    <div
                      key={`${pt.day}-${i}`}
                      style={{
                        flex: 1,
                        display: 'flex',
                        flexDirection: 'column',
                        alignItems: 'center',
                        gap: 4,
                      }}
                      title={`${pt.uptimePct}% uptime`}
                    >
                      <div
                        style={{
                          width: '100%',
                          borderRadius: '6px 6px 0 0',
                          background: barColor(pt.uptimePct),
                          height: barHeight(pt.uptimePct),
                        }}
                      />
                      <div className="text-muted" style={{ fontSize: 10 }}>
                        {pt.day}
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )
        })}
      </Card>

      {selected ? (
        <Dialog title={selected.name} onClose={() => setSelected(null)}>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8, fontSize: 14 }}>
            <Row label="Composite health score" value={selected.score.toFixed(2)} />
            <Row label="AZs healthy" value={selected.azHealthy} />
            <Row label="Traffic share" value={`${selected.trafficPct}%`} />
            <Row label="p99 latency" value={`${selected.latencyMs}ms`} />
            <Row label="Error rate" value={`${selected.errorRatePct}%`} />
            <Row label="Replication lag" value={`${selected.replicationLagMs}ms`} />
            <Row
              label="Last probe"
              value={selected.lastProbe ? relativeTime(selected.lastProbe) : '—'}
            />
          </div>

          <div className="hr" />
          <div className="card-kicker">Dependency breakdown</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginTop: 8 }}>
            {(selected.deps ?? []).map((d) => (
              <div
                key={d.name}
                style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}
              >
                <span style={{ fontSize: 14 }}>{d.name}</span>
                <StatusTag status={d.status} />
              </div>
            ))}
            {(selected.deps ?? []).length === 0 ? (
              <span className="text-muted" style={{ fontSize: 13 }}>
                No dependencies reporting for this region.
              </span>
            ) : null}
          </div>
        </Dialog>
      ) : null}
    </>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between' }}>
      <span>{label}</span>
      <span className="text-muted">{value}</span>
    </div>
  )
}
