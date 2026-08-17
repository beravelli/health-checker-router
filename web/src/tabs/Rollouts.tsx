import { useState } from 'react'
import { Card, EmptyNote, ErrorNote } from '../components/ui'
import { api } from '../lib/api'
import type { Rollout } from '../lib/types'

function statusTag(r: Rollout): { cls: string; label: string } {
  if (r.status === 'done') return { cls: 'status-healthy', label: 'Complete' }
  if (r.paused) return { cls: 'status-degraded', label: 'Paused' }
  return { cls: 'tag-neutral', label: 'In progress' }
}

export function Rollouts({
  rollouts,
  isAdmin,
  onChanged,
}: {
  rollouts: Rollout[]
  isAdmin: boolean
  onChanged: () => void
}) {
  const [busyId, setBusyId] = useState<string | null>(null)
  const [error, setError] = useState<Error | null>(null)

  async function run(id: string, action: () => Promise<unknown>) {
    setBusyId(id)
    setError(null)
    try {
      await action()
      onChanged()
    } catch (err) {
      // A refused resume comes back as 409 with the reason — surfacing it
      // verbatim is the point, since it explains why promotion was blocked.
      setError(err instanceof Error ? err : new Error(String(err)))
    } finally {
      setBusyId(null)
    }
  }

  return (
    <>
      <h1 style={{ marginBottom: 2 }}>Rollouts</h1>
      <p className="text-muted" style={{ marginBottom: 20 }}>
        Deploy progress per region. Canary in one region is verified against the health checker
        before promoting to the other.
      </p>

      {error ? (
        <div style={{ marginBottom: 16 }}>
          <ErrorNote error={error} />
        </div>
      ) : null}

      {rollouts.length === 0 ? (
        <EmptyNote>No rollouts in flight.</EmptyNote>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          {rollouts.map((r) => {
            const tag = statusTag(r)
            const busy = busyId === r.id
            const done = r.status === 'done'
            return (
              <Card key={r.id} elevation="sm" style={{ padding: '18px 20px' }}>
                <div
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    marginBottom: 10,
                    gap: 12,
                  }}
                >
                  <div>
                    <span style={{ fontWeight: 600, fontSize: 14 }}>{r.service || 'unknown service'}</span>
                    <span className="text-muted" style={{ fontSize: 12, marginLeft: 8 }}>
                      {r.fromVersion} → {r.toVersion}
                    </span>
                  </div>
                  <span className={`tag ${tag.cls}`}>{tag.label}</span>
                </div>

                <div className="grid-2" style={{ gap: 16 }}>
                  <Progress label="us-east-1" pct={r.eastPct} />
                  <Progress label="us-west-2" pct={r.westPct} />
                </div>

                <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
                  <button
                    className="btn btn-secondary"
                    disabled={!isAdmin || busy || done}
                    onClick={() =>
                      void run(r.id, () =>
                        r.paused ? api.resumeRollout(r.id) : api.pauseRollout(r.id),
                      )
                    }
                  >
                    {r.paused ? 'Resume' : 'Pause'}
                  </button>
                  <button
                    className="btn btn-ghost"
                    disabled={!isAdmin || busy}
                    onClick={() => void run(r.id, () => api.rollbackRollout(r.id))}
                  >
                    Roll back
                  </button>
                </div>
              </Card>
            )
          })}
        </div>
      )}
    </>
  )
}

function Progress({ label, pct }: { label: string; pct: number }) {
  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginBottom: 4 }}>
        <span>{label}</span>
        <span>{pct}%</span>
      </div>
      <div className="progress">
        <div style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}
