import { useEffect, useState } from 'react'
import { Card, Dialog, ErrorNote } from '../components/ui'
import { api, ApiError } from '../lib/api'
import type {
  FailOpenState,
  Region,
  RoutingEndpointsResponse,
  RoutingProviderId,
  WeightsState,
} from '../lib/types'

const PROVIDER_LABEL: Record<RoutingProviderId, string> = {
  aws_native: 'AWS Route 53 / ALB-NLB',
  cloudfront: 'AWS CloudFront',
  avi_gslb: 'AVI GSLB',
  akamai_cloudlet: 'Akamai Cloudlet',
  aws_arc: 'AWS Application Recovery Controller',
}

/**
 * The edge cases this system is designed around. These are documentation, not
 * data — they describe deliberate behaviour, so they live with the UI rather
 * than in the database where they would imply they are configurable.
 */
const EDGE_CASES = [
  {
    id: 'ec1',
    title: 'Single dependency degraded, service otherwise healthy',
    description:
      'A service reports healthy liveness but one dependency (e.g. cache) is degraded. The checker weights dependency health into the service score instead of a binary up/down, so partial degradation shows without a false full-outage signal.',
    mitigation: 'Composite health score per service; LB weight reduces proportionally, not to zero.',
  },
  {
    id: 'ec2',
    title: 'Flapping health checks',
    description:
      'A probe alternates pass/fail near a threshold, which would otherwise cause the LB to rapidly flip a region in and out of rotation.',
    mitigation:
      'Require 3 consecutive failures (and 2 consecutive successes to recover) before changing routing weight — hysteresis, not single-probe decisions.',
  },
  {
    id: 'ec3',
    title: 'Network partition between regions',
    description:
      "us-east-1 and us-west-2 cannot see each other, but each is independently healthy to its own clients. Naive cross-region checks would both report the other as 'down'.",
    mitigation:
      'Health is judged locally per region against real client traffic (synthetic + real error rates), not by regions checking each other — avoids split-brain false failovers.',
  },
  {
    id: 'ec4',
    title: 'DNS TTL propagation delay',
    description:
      'On a full regional failover, clients cached at a stale DNS TTL keep sending to the pulled region for up to the TTL window.',
    mitigation:
      'LB-level weight change (Envoy) takes effect immediately at the edge, independent of DNS; DNS TTL is kept at 30s as a backstop, not the primary mechanism.',
  },
  {
    id: 'ec5',
    title: 'Cascading retries under partial degradation',
    description:
      'Clients and internal callers retrying against a degraded-but-not-down region amplify load and can tip it into full failure.',
    mitigation:
      'Circuit breaker + capped retry budget at the Envoy layer; degraded status disables client-side retry-to-same-region.',
  },
  {
    id: 'ec6',
    title: 'Inconclusive health signal (fail-open)',
    description:
      "Probes time out or quorum can't be reached — this is genuinely unknown, not confirmed-down.",
    mitigation:
      'LB fails open on last-known-good weights for up to 10 minutes and pages on-call, rather than guessing and pulling a possibly-healthy region.',
  },
]

export function Failover({
  regions,
  weights,
  failOpen,
  routing,
  isAdmin,
  onChanged,
}: {
  regions: Region[]
  weights: WeightsState
  failOpen: FailOpenState
  routing: RoutingEndpointsResponse
  isAdmin: boolean
  onChanged: () => void
}) {
  const [draft, setDraft] = useState<Record<string, number>>(weights.weights)
  const [expanded, setExpanded] = useState<string | null>('ec2')
  const [confirming, setConfirming] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error | null>(null)

  // Re-sync the sliders when the server pushes a new split, so a weight
  // changed by another operator (or by automatic reweighting) is reflected
  // rather than silently overwritten by this tab's stale draft.
  useEffect(() => setDraft(weights.weights), [weights.weights])

  const regionIds = regions.map((r) => r.id)
  const active = (routing.endpoints ?? []).find((e) => e.enabled)
  const activeLabel = active ? PROVIDER_LABEL[active.provider] : 'none configured'

  /**
   * Sliders are linked: raising one lowers the others so the split always sums
   * to 100. With more than two regions the remainder is spread evenly across
   * the rest, which keeps the invariant without needing per-region locks.
   */
  function setWeight(regionId: string, value: number) {
    const others = regionIds.filter((id) => id !== regionId)
    if (others.length === 0) return

    const remaining = 100 - value
    const next: Record<string, number> = { [regionId]: value }

    const currentOthersTotal = others.reduce((sum, id) => sum + (draft[id] ?? 0), 0)
    others.forEach((id, i) => {
      if (currentOthersTotal === 0) {
        next[id] = i === 0 ? remaining : 0
      } else {
        next[id] = Math.round(((draft[id] ?? 0) / currentOthersTotal) * remaining)
      }
    })

    // Rounding can leave the total a point or two off; absorb it in the first
    // of the other regions.
    const total = Object.values(next).reduce((a, b) => a + b, 0)
    if (total !== 100 && others.length > 0) {
      next[others[0]] = (next[others[0]] ?? 0) + (100 - total)
    }
    setDraft(next)
  }

  async function run(action: () => Promise<unknown>) {
    setBusy(true)
    setError(null)
    try {
      await action()
      onChanged()
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)))
    } finally {
      setBusy(false)
    }
  }

  const dirty = regionIds.some((id) => (draft[id] ?? 0) !== (weights.weights[id] ?? 0))

  return (
    <>
      <h1 style={{ marginBottom: 2 }}>Failover &amp; routing</h1>
      <p className="text-muted" style={{ marginBottom: 20 }}>
        Envoy edge health checks drive load-balancer weights. Manual override is available for
        confirmed regional incidents.
      </p>

      {error ? (
        <div style={{ marginBottom: 20 }}>
          <ErrorNote
            error={
              error instanceof ApiError && error.isForbidden
                ? new Error(
                    `${error.message}${error.requiredGroup ? ` (${error.requiredGroup})` : ''}`,
                  )
                : error
            }
          />
        </div>
      ) : null}

      <Card
        elevation="sm"
        style={{
          padding: '16px 20px',
          marginBottom: 20,
          display: 'flex',
          alignItems: 'center',
          gap: 10,
          flexWrap: 'wrap',
        }}
      >
        <span className="text-muted" style={{ fontSize: 13 }}>
          Routing via
        </span>
        <span className="tag tag-accent-2">{activeLabel}</span>
        <span className="text-muted" style={{ fontSize: 12 }}>
          — configured in Admin
        </span>
        <span style={{ marginLeft: 'auto' }} className="tag tag-neutral">
          {weights.automatic ? 'Automatic health-based weighting' : 'Manual override active'}
        </span>
      </Card>

      <h2 style={{ marginTop: 0 }}>Routing endpoints</h2>
      <Card elevation="sm" style={{ padding: 0, overflow: 'hidden', marginBottom: 28 }}>
        <div style={{ overflowX: 'auto' }}>
          <table className="table">
            <thead>
              <tr>
                <th>Provider</th>
                <th>Status</th>
                <th>Config</th>
                <th>Auth</th>
              </tr>
            </thead>
            <tbody>
              {(['aws_native', 'cloudfront', 'avi_gslb', 'akamai_cloudlet', 'aws_arc'] as RoutingProviderId[]).map(
                (id) => {
                  const ep = (routing.endpoints ?? []).find((e) => e.provider === id)
                  const configured = ep ? Object.keys(ep.config ?? {}).length > 0 : false
                  const label = ep?.enabled ? 'Active' : configured ? 'Configured' : 'Not configured'
                  const cls = ep?.enabled ? 'tag-accent-2' : 'tag-neutral'
                  return (
                    <tr key={id}>
                      <td style={{ fontWeight: 600 }}>{PROVIDER_LABEL[id]}</td>
                      <td>
                        <span className={`tag ${cls}`}>{label}</span>
                      </td>
                      <td className="text-muted" style={{ fontSize: 13 }}>
                        {configured ? summarizeConfig(ep!.config) : 'Not configured'}
                      </td>
                      <td className="text-muted" style={{ fontSize: 13 }}>
                        {ep?.authRef ? `${ep.authMethod ?? 'ref'}: ${ep.authRef}` : 'Not set'}
                      </td>
                    </tr>
                  )
                },
              )}
            </tbody>
          </table>
        </div>
      </Card>

      <div style={{ display: 'grid', gridTemplateColumns: '1.2fr 1fr', gap: 20, alignItems: 'start' }}>
        <Card elevation="md">
          <h3 style={{ marginBottom: 4 }}>Load balancer weights</h3>
          <p className="card-body" style={{ marginBottom: 16 }}>
            Traffic is split by health-checked weight, not fixed percentage. Dragging overrides the
            automatic health-based weighting until reset.
          </p>

          {regions.map((r) => (
            <div key={r.id} style={{ marginBottom: 18 }}>
              <div
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  fontSize: 13,
                  marginBottom: 6,
                }}
              >
                <span>{r.displayName}</span>
                <span style={{ fontFamily: 'var(--font-heading)' }}>{draft[r.id] ?? 0}%</span>
              </div>
              <input
                type="range"
                min={0}
                max={100}
                value={draft[r.id] ?? 0}
                disabled={!isAdmin || busy}
                onChange={(e) => setWeight(r.id, Number(e.target.value))}
                style={{ width: '100%' }}
                aria-label={`${r.displayName} traffic weight`}
              />
            </div>
          ))}

          <div style={{ display: 'flex', gap: 10, marginTop: 14, flexWrap: 'wrap' }}>
            <button
              className="btn btn-secondary"
              disabled={!isAdmin || busy}
              onClick={() => void run(() => api.resetWeights())}
            >
              Reset to automatic
            </button>
            <button
              className="btn btn-secondary"
              disabled={!isAdmin || busy || !dirty}
              onClick={() => void run(() => api.setWeights(draft))}
            >
              Apply weights
            </button>
            {regions.map((r) => (
              <button
                key={r.id}
                className="btn btn-primary"
                disabled={!isAdmin || busy}
                onClick={() => setConfirming(r.id)}
              >
                Fail over to {r.displayName}
              </button>
            ))}
          </div>

          {!isAdmin ? (
            <p className="text-muted" style={{ fontSize: 12, marginTop: 12, marginBottom: 0 }}>
              Read-only — changing routing requires admin group membership.
            </p>
          ) : null}
        </Card>

        <Card elevation="md">
          <h3 style={{ marginBottom: 4 }}>Fail-open policy</h3>
          <p className="card-body">
            When health-check quorum across probes is inconclusive (e.g. probe timeout, partial
            network partition) rather than a confirmed failure, the LB fails open — it keeps routing
            to last-known-good weights instead of pulling a region fully out, to avoid a
            false-positive full outage.
          </p>
          <div className="hr" />
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8, fontSize: 13 }}>
            <PolicyRow label="Probe interval" value={`${failOpen.policy.probeIntervalSeconds}s`} />
            <PolicyRow
              label="Consecutive failures to trip"
              value={String(failOpen.policy.failuresToTrip)}
            />
            <PolicyRow
              label="Consecutive successes to recover"
              value={String(failOpen.policy.successToRecover)}
            />
            <PolicyRow
              label="Quorum required"
              value={`${failOpen.policy.quorumRequired} of ${failOpen.policy.quorumTotal} AZ probes`}
            />
            <PolicyRow
              label="Fail-open hold"
              value={`last-known-good, max ${Math.round(failOpen.policy.holdSeconds / 60)}m`}
            />
          </div>
        </Card>
      </div>

      <h2 style={{ marginTop: 32 }}>Edge cases this system covers</h2>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {EDGE_CASES.map((e) => {
          const open = expanded === e.id
          return (
            <Card key={e.id} elevation="sm" style={{ padding: 0 }}>
              <button
                onClick={() => setExpanded(open ? null : e.id)}
                aria-expanded={open}
                style={{
                  all: 'unset',
                  boxSizing: 'border-box',
                  width: '100%',
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  gap: 12,
                  padding: '16px 20px',
                  cursor: 'pointer',
                }}
              >
                <span style={{ fontWeight: 600, fontSize: 14 }}>{e.title}</span>
                <span className="text-muted" style={{ fontSize: 18 }}>
                  {open ? '−' : '+'}
                </span>
              </button>
              {open ? (
                <div style={{ padding: '0 20px 18px', fontSize: 13 }}>
                  <p className="text-muted" style={{ marginBottom: 8 }}>
                    {e.description}
                  </p>
                  <div style={{ display: 'flex', gap: 8, alignItems: 'flex-start' }}>
                    <span className="tag tag-accent-2" style={{ flex: 'none' }}>
                      Mitigation
                    </span>
                    <span>{e.mitigation}</span>
                  </div>
                </div>
              ) : null}
            </Card>
          )
        })}
      </div>

      {confirming ? (
        <Dialog
          title="Confirm regional failover"
          onClose={() => setConfirming(null)}
          footer={
            <>
              <button className="btn btn-ghost" onClick={() => setConfirming(null)}>
                Cancel
              </button>
              <button
                className="btn btn-primary"
                disabled={busy}
                onClick={() => {
                  const region = confirming
                  setConfirming(null)
                  void run(() => api.failover(region))
                }}
              >
                Fail over to {confirming}
              </button>
            </>
          }
        >
          <p style={{ fontSize: 14 }}>
            Confirming will route <strong>100% of traffic to {confirming}</strong>, pull every other
            region out of rotation, and page on-call in all regions.
          </p>
          <p className="text-muted" style={{ fontSize: 13, marginBottom: 0 }}>
            This also switches weighting to manual. Automatic health-based weighting stays off until
            you reset it.
          </p>
        </Dialog>
      ) : null}
    </>
  )
}

function PolicyRow({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between' }}>
      <span>{label}</span>
      <span className="text-muted">{value}</span>
    </div>
  )
}

function summarizeConfig(config: Record<string, unknown>): string {
  const parts = Object.entries(config)
    .filter(([, v]) => typeof v === 'string' && v)
    .slice(0, 2)
    .map(([k, v]) => `${k}: ${String(v)}`)
  return parts.length > 0 ? parts.join(' · ') : 'Configured'
}
