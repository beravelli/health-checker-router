import { Card, EmptyNote, StatusTag } from '../components/ui'
import type { HealthSnapshot, Region } from '../lib/types'

export function Services({
  snapshot,
  regions,
}: {
  snapshot: HealthSnapshot
  regions: Region[]
}) {
  const services = snapshot.services ?? []

  return (
    <>
      <h1 style={{ marginBottom: 2 }}>Service matrix</h1>
      <p className="text-muted" style={{ marginBottom: 20 }}>
        Per-service health across both sites, including deploy version skew.
      </p>

      {services.length === 0 ? (
        <EmptyNote>
          No services are registered yet. Add one in <strong>Admin</strong> to start probing.
        </EmptyNote>
      ) : (
        <Card elevation="sm" style={{ padding: 0, overflow: 'hidden' }}>
          <div style={{ overflowX: 'auto' }}>
            <table className="table">
              <thead>
                <tr>
                  <th>Service</th>
                  <th>Runtime</th>
                  {regions.map((r) => (
                    <th key={r.id}>{r.displayName}</th>
                  ))}
                  <th>Version skew</th>
                  <th>Notes</th>
                </tr>
              </thead>
              <tbody>
                {services.map((s) => (
                  <tr key={s.serviceId}>
                    <td style={{ fontWeight: 600 }}>{s.name}</td>
                    <td className="text-muted">{s.runtime || '—'}</td>
                    {regions.map((r) => {
                      const status = s.byRegion?.[r.id]
                      return (
                        <td key={r.id}>
                          {status ? (
                            <StatusTag status={status} />
                          ) : (
                            <span className="text-muted">not deployed</span>
                          )}
                        </td>
                      )
                    })}
                    <td>
                      {s.skew ? (
                        <span className="tag tag-outline">skewed</span>
                      ) : (
                        <span className="text-muted">—</span>
                      )}
                    </td>
                    <td className="text-muted" style={{ fontSize: 13 }}>
                      {s.note}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </>
  )
}
