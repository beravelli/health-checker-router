import { Card, EmptyNote, Segmented, clockTime, relativeTime } from '../components/ui'
import type { Incident, Severity } from '../lib/types'

const SEV_META: Record<Severity, { cls: string; dot: string; label: string }> = {
  critical: { cls: 'status-critical', dot: 'var(--color-critical-base)', label: 'Critical' },
  warning: { cls: 'status-degraded', dot: 'var(--color-accent-500)', label: 'Warning' },
  resolved: { cls: 'status-healthy', dot: 'var(--color-accent-2-500)', label: 'Resolved' },
}

export function Incidents({
  incidents,
  filter,
  onFilterChange,
}: {
  incidents: Incident[]
  filter: string
  onFilterChange: (f: string) => void
}) {
  return (
    <>
      <div
        style={{
          display: 'flex',
          alignItems: 'flex-end',
          justifyContent: 'space-between',
          gap: 16,
          flexWrap: 'wrap',
          marginBottom: 20,
        }}
      >
        <div>
          <h1 style={{ marginBottom: 2 }}>Incidents</h1>
          <p className="text-muted" style={{ margin: 0 }}>
            Cross-region incident and notification timeline.
          </p>
        </div>
        <Segmented
          name="severity"
          value={filter}
          onChange={onFilterChange}
          options={[
            { id: 'all', label: 'All' },
            { id: 'critical', label: 'Critical' },
            { id: 'warning', label: 'Warning' },
            { id: 'resolved', label: 'Resolved' },
          ]}
        />
      </div>

      {incidents.length === 0 ? (
        <EmptyNote>
          {filter === 'all'
            ? 'No incidents recorded. Quiet is good.'
            : `No ${filter} incidents in the current window.`}
        </EmptyNote>
      ) : (
        <Card elevation="sm" style={{ padding: '8px 0' }}>
          {incidents.map((i) => {
            const meta = SEV_META[i.severity] ?? SEV_META.warning
            return (
              <div
                key={i.id}
                style={{ display: 'flex', gap: 14, padding: '14px 20px', alignItems: 'flex-start' }}
              >
                <div style={{ width: 78, flex: 'none' }}>
                  <div style={{ fontSize: 13, fontWeight: 600 }}>{clockTime(i.occurredAt)}</div>
                  <div className="text-muted" style={{ fontSize: 11 }}>
                    {relativeTime(i.occurredAt)}
                  </div>
                </div>

                <div
                  style={{
                    width: 4,
                    alignSelf: 'stretch',
                    borderRadius: 999,
                    background: meta.dot,
                    flex: 'none',
                  }}
                />

                <div style={{ minWidth: 0 }}>
                  <div
                    style={{
                      display: 'flex',
                      gap: 8,
                      alignItems: 'center',
                      flexWrap: 'wrap',
                      marginBottom: 4,
                    }}
                  >
                    <span style={{ fontWeight: 600, fontSize: 14 }}>{i.title}</span>
                    <span className={`tag ${meta.cls}`}>{meta.label}</span>
                    {i.regionId ? <span className="tag tag-neutral">{i.regionId}</span> : null}
                  </div>
                  <div className="text-muted" style={{ fontSize: 13 }}>
                    {i.description}
                  </div>
                </div>
              </div>
            )
          })}
        </Card>
      )}
    </>
  )
}
