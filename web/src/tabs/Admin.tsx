import { useState } from 'react'
import { Card, ErrorNote, Field, Segmented } from '../components/ui'
import { api } from '../lib/api'
import type {
  AWSAuthMethod,
  ProbeType,
  Region,
  RoutingEndpointsResponse,
  RoutingProviderId,
  Service,
} from '../lib/types'

const PROVIDER_LABEL: Record<RoutingProviderId, string> = {
  aws_native: 'AWS Route 53 / ALB-NLB',
  cloudfront: 'AWS CloudFront',
  avi_gslb: 'AVI GSLB',
  akamai_cloudlet: 'Akamai Cloudlet',
  aws_arc: 'AWS Application Recovery Controller',
}

const PROBE_LABEL: Record<ProbeType, string> = {
  http: 'HTTP(S)',
  tcp: 'TCP',
  aws_alb: 'AWS ALB',
  aws_nlb: 'AWS NLB',
  grpc: 'gRPC',
  custom: 'Custom',
}

const AUTH_LABEL: Record<AWSAuthMethod, string> = {
  assume_role: 'Assume IAM role',
  service_account: 'Service account',
  aws_sso: 'AWS SSO',
  custom_sso_binary: 'Custom SSO binary',
}

/** AccessDenied mirrors the server's 403, including the group it named. */
export function AccessDenied({ group, currentGroups }: { group: string; currentGroups: string[] }) {
  return (
    <>
      <h1 style={{ marginBottom: 2 }}>Admin</h1>
      <p className="text-muted" style={{ marginBottom: 20 }}>
        Register services for the health checker to probe, and control whether each one can drive LB
        failover decisions.
      </p>
      <Card elevation="md" style={{ padding: 26 }}>
        <div className="card-kicker">Access restricted</div>
        <div className="card-title" style={{ fontSize: 20, marginBottom: 8 }}>
          You don't have admin access to this section
        </div>
        <p className="card-body" style={{ marginBottom: 14 }}>
          Editing checked services and their failover role requires LDAP group membership in{' '}
          <span className="tag tag-accent">{group}</span>. You currently resolve to{' '}
          {currentGroups.length > 0 ? (
            currentGroups.map((g) => (
              <span key={g} className="tag tag-neutral" style={{ marginRight: 4 }}>
                {g}
              </span>
            ))
          ) : (
            <span className="tag tag-neutral">no groups</span>
          )}
          , which can view health data but not modify it.
        </p>
        <p className="text-muted" style={{ fontSize: 12, marginBottom: 0 }}>
          Ask an existing member of {group} to add you, or request access through your identity team.
        </p>
      </Card>
    </>
  )
}

interface FormState {
  name: string
  runtime: string
  role: 'healthcheck_only' | 'healthcheck_failover'
  probeType: ProbeType
  regions: Record<string, boolean>
  endpoints: Record<string, string>
  httpPath: string
  expectedStatus: string
  tcpPort: string
  targetGroupArn: string
  grpcService: string
  customCommand: string
  intervalS: string
  timeoutS: string
  failuresToTrip: string
  successToRecover: string
  caCertSecretEnv: string
  awsAuthMethod: AWSAuthMethod
  awsAuthRef: string
  awsExternalId: string
}

function emptyForm(regions: Region[]): FormState {
  return {
    name: '',
    runtime: '',
    // Default to monitor-only: registering a service must not silently give it
    // the power to move production traffic.
    role: 'healthcheck_only',
    probeType: 'http',
    regions: Object.fromEntries(regions.map((r) => [r.id, true])),
    endpoints: Object.fromEntries(regions.map((r) => [r.id, ''])),
    httpPath: '/healthz',
    expectedStatus: '200',
    tcpPort: '',
    targetGroupArn: '',
    grpcService: 'grpc.health.v1.Health',
    customCommand: '',
    intervalS: '5',
    timeoutS: '3',
    failuresToTrip: '3',
    successToRecover: '2',
    caCertSecretEnv: '',
    awsAuthMethod: 'assume_role',
    awsAuthRef: '',
    awsExternalId: '',
  }
}

export function Admin({
  regions,
  services,
  routing,
  onChanged,
}: {
  regions: Region[]
  services: Service[]
  routing: RoutingEndpointsResponse
  onChanged: () => void
}) {
  const [form, setForm] = useState<FormState>(() => emptyForm(regions))
  const [error, setError] = useState<Error | null>(null)
  const [busy, setBusy] = useState(false)

  const [regionForm, setRegionForm] = useState({ id: '', displayName: '', provider: 'AWS', zoneLabel: '' })
  const [provider, setProvider] = useState<RoutingProviderId>(
    (routing.endpoints ?? []).find((e) => e.enabled)?.provider ?? 'aws_native',
  )
  const [routingCfg, setRoutingCfg] = useState('')
  const [routingAuthRef, setRoutingAuthRef] = useState('')
  const [routingAuthMethod, setRoutingAuthMethod] = useState('assume_role')

  const set = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((f) => ({ ...f, [key]: value }))

  async function run(action: () => Promise<unknown>, after?: () => void) {
    setBusy(true)
    setError(null)
    try {
      await action()
      after?.()
      onChanged()
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)))
    } finally {
      setBusy(false)
    }
  }

  function submitService() {
    const chosen = Object.entries(form.regions)
      .filter(([, on]) => on)
      .map(([id]) => id)

    if (!form.name.trim()) {
      setError(new Error('Service name is required'))
      return
    }
    if (chosen.length === 0) {
      setError(new Error('Select at least one region'))
      return
    }

    const probeConfig: Record<string, unknown> = {}
    switch (form.probeType) {
      case 'http':
        probeConfig.path = form.httpPath
        probeConfig.expectedStatus = Number(form.expectedStatus) || 200
        break
      case 'tcp':
        probeConfig.port = Number(form.tcpPort) || 0
        break
      case 'aws_alb':
      case 'aws_nlb':
        probeConfig.targetGroupArn = form.targetGroupArn
        if (form.awsExternalId) probeConfig.awsExternalId = form.awsExternalId
        break
      case 'grpc':
        probeConfig.grpcService = form.grpcService
        break
      case 'custom':
        probeConfig.command = form.customCommand
        break
    }

    const isAws = form.probeType === 'aws_alb' || form.probeType === 'aws_nlb'

    void run(
      () =>
        api.createService({
          name: form.name.trim(),
          runtime: form.runtime.trim(),
          role: form.role,
          probeType: form.probeType,
          probeConfig,
          probeIntervalS: Number(form.intervalS) || 5,
          probeTimeoutS: Number(form.timeoutS) || 3,
          consecutiveFailuresToTrip: Number(form.failuresToTrip) || 3,
          consecutiveSuccessToRecover: Number(form.successToRecover) || 2,
          caCertSecretEnv: form.caCertSecretEnv,
          awsAuthMethod: isAws ? form.awsAuthMethod : '',
          awsAuthRef: isAws ? form.awsAuthRef : '',
          regions: Object.fromEntries(chosen.map((id) => [id, form.endpoints[id] ?? ''])),
          versions: {},
        }),
      () => setForm(emptyForm(regions)),
    )
  }

  return (
    <>
      <h1 style={{ marginBottom: 2 }}>Admin</h1>
      <p className="text-muted" style={{ marginBottom: 20 }}>
        Register services for the health checker to probe, and control whether each one can drive LB
        failover decisions or is monitoring-only.
      </p>

      {error ? (
        <div style={{ marginBottom: 20 }}>
          <ErrorNote error={error} />
        </div>
      ) : null}

      {/* ---------------------------------------------------- regions */}
      <Card elevation="md" style={{ marginBottom: 24 }}>
        <h3 style={{ marginBottom: 4 }}>Regions</h3>
        <p className="card-body" style={{ marginBottom: 14 }}>
          Sites the checker probes. Removing a region deletes its probe history and any service
          bindings scoped to it.
        </p>

        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr 1fr auto', gap: 12, alignItems: 'end' }}>
          <Field label="Region id">
            <input
              type="text"
              placeholder="eu-west-1"
              value={regionForm.id}
              onChange={(e) => setRegionForm({ ...regionForm, id: e.target.value })}
            />
          </Field>
          <Field label="Display name">
            <input
              type="text"
              placeholder="eu-west-1"
              value={regionForm.displayName}
              onChange={(e) => setRegionForm({ ...regionForm, displayName: e.target.value })}
            />
          </Field>
          <Field label="Provider">
            <input
              type="text"
              value={regionForm.provider}
              onChange={(e) => setRegionForm({ ...regionForm, provider: e.target.value })}
            />
          </Field>
          <Field label="Zone label">
            <input
              type="text"
              placeholder="primary write region"
              value={regionForm.zoneLabel}
              onChange={(e) => setRegionForm({ ...regionForm, zoneLabel: e.target.value })}
            />
          </Field>
          <div className="field">
            <button
              className="btn btn-primary"
              disabled={busy || !regionForm.id.trim()}
              onClick={() =>
                void run(
                  () => api.upsertRegion(regionForm),
                  () => setRegionForm({ id: '', displayName: '', provider: 'AWS', zoneLabel: '' }),
                )
              }
            >
              Add region
            </button>
          </div>
        </div>

        <table className="table" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>Region</th>
              <th>Provider</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {regions.map((r) => (
              <tr key={r.id}>
                <td style={{ fontWeight: 600 }}>{r.displayName}</td>
                <td className="text-muted">{r.provider}</td>
                <td style={{ textAlign: 'right' }}>
                  <button
                    className="btn btn-ghost"
                    disabled={busy}
                    onClick={() => void run(() => api.deleteRegion(r.id))}
                  >
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      {/* --------------------------------------------- add a service */}
      <Card elevation="md" style={{ marginBottom: 24 }}>
        <h3 style={{ marginBottom: 4 }}>Add a service</h3>
        <p className="card-body" style={{ marginBottom: 14 }}>
          New services default to <strong>health check only</strong>. Promote to failover once
          you've confirmed the probe behaves as expected — a misconfigured probe with failover
          rights can move production traffic.
        </p>

        <div className="grid-2">
          <Field label="Name">
            <input
              type="text"
              placeholder="checkout-service"
              value={form.name}
              onChange={(e) => set('name', e.target.value)}
            />
          </Field>
          <Field label="Runtime">
            <input
              type="text"
              placeholder="EC2 ASG"
              value={form.runtime}
              onChange={(e) => set('runtime', e.target.value)}
            />
          </Field>
        </div>

        <Field label="Role">
          <Segmented
            name="role"
            value={form.role}
            onChange={(v) => set('role', v)}
            options={[
              { id: 'healthcheck_only', label: 'Health check only' },
              { id: 'healthcheck_failover', label: 'Health check + failover' },
            ]}
          />
        </Field>

        <Field label="Regions" hint="Endpoint is the base URL or host the probe targets.">
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            {regions.map((r) => (
              <div key={r.id} style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
                <label className="checkbox" style={{ minWidth: 130 }}>
                  <input
                    type="checkbox"
                    checked={form.regions[r.id] ?? false}
                    onChange={(e) => set('regions', { ...form.regions, [r.id]: e.target.checked })}
                  />
                  {r.displayName}
                </label>
                <input
                  type="text"
                  placeholder={`https://${r.id}.internal.example.com`}
                  value={form.endpoints[r.id] ?? ''}
                  disabled={!form.regions[r.id]}
                  onChange={(e) => set('endpoints', { ...form.endpoints, [r.id]: e.target.value })}
                  style={{
                    flex: 1,
                    fontFamily: 'var(--font-body)',
                    fontSize: 14,
                    padding: '10px 16px',
                    borderRadius: 'var(--radius-pill)',
                    border: '1.5px solid var(--color-neutral-300)',
                    background: 'var(--color-neutral-100)',
                  }}
                />
              </div>
            ))}
          </div>
        </Field>

        <Field label="Check type">
          <Segmented
            name="probeType"
            fullWidth
            value={form.probeType}
            onChange={(v) => set('probeType', v)}
            options={[
              { id: 'http', label: 'HTTP(S)' },
              { id: 'tcp', label: 'TCP' },
              { id: 'aws_alb', label: 'AWS ALB' },
              { id: 'aws_nlb', label: 'AWS NLB' },
              { id: 'grpc', label: 'gRPC' },
              { id: 'custom', label: 'Custom' },
            ]}
          />
        </Field>

        {form.probeType === 'http' ? (
          <div className="grid-2">
            <Field label="Path">
              <input type="text" value={form.httpPath} onChange={(e) => set('httpPath', e.target.value)} />
            </Field>
            <Field label="Expected status">
              <input
                type="number"
                value={form.expectedStatus}
                onChange={(e) => set('expectedStatus', e.target.value)}
              />
            </Field>
          </div>
        ) : null}

        {form.probeType === 'tcp' ? (
          <Field label="Port">
            <input type="number" value={form.tcpPort} onChange={(e) => set('tcpPort', e.target.value)} />
          </Field>
        ) : null}

        {form.probeType === 'grpc' ? (
          <Field label="gRPC service">
            <input
              type="text"
              value={form.grpcService}
              onChange={(e) => set('grpcService', e.target.value)}
            />
          </Field>
        ) : null}

        {form.probeType === 'custom' ? (
          <Field
            label="Command"
            hint="Must be listed in SSO_BINARY_ALLOWLIST. Exit 0 = healthy, 1 = degraded, other = critical."
          >
            <input
              type="text"
              placeholder="/opt/checks/verify-search.sh"
              value={form.customCommand}
              onChange={(e) => set('customCommand', e.target.value)}
            />
          </Field>
        ) : null}

        {form.probeType === 'aws_alb' || form.probeType === 'aws_nlb' ? (
          <>
            <Field label="Target group ARN">
              <input
                type="text"
                placeholder="arn:aws:elasticloadbalancing:us-east-1:…:targetgroup/…"
                value={form.targetGroupArn}
                onChange={(e) => set('targetGroupArn', e.target.value)}
              />
            </Field>
            <Field label="AWS authentication">
              <Segmented
                name="awsAuth"
                fullWidth
                value={form.awsAuthMethod}
                onChange={(v) => set('awsAuthMethod', v)}
                options={[
                  { id: 'assume_role', label: 'Assume IAM role' },
                  { id: 'service_account', label: 'Service account' },
                  { id: 'aws_sso', label: 'AWS SSO' },
                  { id: 'custom_sso_binary', label: 'Custom SSO binary' },
                ]}
              />
            </Field>
            <Field
              label={
                form.awsAuthMethod === 'assume_role'
                  ? 'Role ARN'
                  : form.awsAuthMethod === 'service_account'
                    ? 'Secret env var name'
                    : form.awsAuthMethod === 'aws_sso'
                      ? 'SSO profile name'
                      : 'Binary path'
              }
              hint="Stored as a reference. The credential value itself never enters the database."
            >
              <input
                type="text"
                value={form.awsAuthRef}
                onChange={(e) => set('awsAuthRef', e.target.value)}
              />
            </Field>
            {form.awsAuthMethod === 'assume_role' ? (
              <Field label="External ID (optional)">
                <input
                  type="text"
                  value={form.awsExternalId}
                  onChange={(e) => set('awsExternalId', e.target.value)}
                />
              </Field>
            ) : null}
          </>
        ) : null}

        <div className="hr" />
        <div className="grid-4">
          <Field label="Interval (s)">
            <input type="number" value={form.intervalS} onChange={(e) => set('intervalS', e.target.value)} />
          </Field>
          <Field label="Timeout (s)">
            <input type="number" value={form.timeoutS} onChange={(e) => set('timeoutS', e.target.value)} />
          </Field>
          <Field label="Failures to trip">
            <input
              type="number"
              value={form.failuresToTrip}
              onChange={(e) => set('failuresToTrip', e.target.value)}
            />
          </Field>
          <Field label="Successes to recover">
            <input
              type="number"
              value={form.successToRecover}
              onChange={(e) => set('successToRecover', e.target.value)}
            />
          </Field>
        </div>

        <Field
          label="CA bundle env var (optional)"
          hint="Name of an environment variable holding a PEM bundle. If set and unresolvable, the probe refuses to run rather than falling back to the system trust store."
        >
          <input
            type="text"
            placeholder="CHECKOUT_CA_BUNDLE"
            value={form.caCertSecretEnv}
            onChange={(e) => set('caCertSecretEnv', e.target.value)}
          />
        </Field>

        <button className="btn btn-primary" disabled={busy} onClick={submitService}>
          Add service
        </button>
      </Card>

      {/* ------------------------------------------ configured services */}
      <Card elevation="md" style={{ marginBottom: 24, padding: 0, overflow: 'hidden' }}>
        <div style={{ padding: '22px 22px 0' }}>
          <h3 style={{ marginBottom: 4 }}>Configured services</h3>
          <p className="card-body">
            Click a role tag to toggle between monitoring-only and failover-capable.
          </p>
        </div>
        <div style={{ overflowX: 'auto' }}>
          <table className="table">
            <thead>
              <tr>
                <th>Service</th>
                <th>Runtime</th>
                <th>Regions</th>
                <th>Check</th>
                <th>Timing</th>
                <th>Role</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {services.map((s) => (
                <tr key={s.id}>
                  <td style={{ fontWeight: 600 }}>{s.name}</td>
                  <td className="text-muted">{s.runtime || '—'}</td>
                  <td className="text-muted">{(s.regions ?? []).map((r) => r.regionId).join(', ')}</td>
                  <td className="text-muted" style={{ fontSize: 13 }}>
                    {PROBE_LABEL[s.probeType]}
                    {s.caCertSecretEnv ? ' · custom CA' : ''}
                    {s.awsAuthMethod ? ` · ${AUTH_LABEL[s.awsAuthMethod]}` : ''}
                  </td>
                  <td className="text-muted" style={{ fontSize: 13 }}>
                    every {s.probeIntervalS}s, {s.probeTimeoutS}s timeout ·{' '}
                    {s.consecutiveFailuresToTrip} down / {s.consecutiveSuccessToRecover} up
                  </td>
                  <td>
                    <button
                      className={`tag ${s.role === 'healthcheck_failover' ? 'tag-accent-2' : 'tag-neutral'}`}
                      style={{ border: 0, cursor: 'pointer' }}
                      disabled={busy}
                      title="Toggle failover eligibility"
                      onClick={() =>
                        void run(() =>
                          api.updateServiceRole(
                            s.id,
                            s.role === 'healthcheck_failover'
                              ? 'healthcheck_only'
                              : 'healthcheck_failover',
                          ),
                        )
                      }
                    >
                      {s.role === 'healthcheck_failover' ? 'Failover' : 'Monitor only'}
                    </button>
                  </td>
                  <td style={{ textAlign: 'right' }}>
                    <button
                      className="btn btn-ghost"
                      disabled={busy}
                      onClick={() => void run(() => api.deleteService(s.id))}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
              {services.length === 0 ? (
                <tr>
                  <td colSpan={7} className="text-muted">
                    No services registered yet.
                  </td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </Card>

      {/* ------------------------------------------- routing provider */}
      <Card elevation="md">
        <h3 style={{ marginBottom: 4 }}>Traffic routing provider</h3>
        <p className="card-body" style={{ marginBottom: 14 }}>
          Which traffic manager fronts this app pair. The checker pushes weight and failover changes
          here. Enabling one provider disables the others — exactly one fronts the app at a time.
        </p>

        <Field label="Provider">
          <Segmented
            name="routingProvider"
            fullWidth
            value={provider}
            onChange={setProvider}
            options={(Object.keys(PROVIDER_LABEL) as RoutingProviderId[]).map((id) => ({
              id,
              label: PROVIDER_LABEL[id],
            }))}
          />
        </Field>

        <Field
          label="Provider config (JSON)"
          hint={configHint(provider)}
        >
          <textarea
            value={routingCfg}
            placeholder={configPlaceholder(provider)}
            onChange={(e) => setRoutingCfg(e.target.value)}
          />
        </Field>

        {provider === 'aws_native' || provider === 'cloudfront' ? (
          <Field label="Auth method">
            <Segmented
              name="routingAuthMethod"
              fullWidth
              value={routingAuthMethod}
              onChange={setRoutingAuthMethod}
              options={[
                { id: 'assume_role', label: 'Assume IAM role' },
                { id: 'service_account', label: 'Service account' },
                { id: 'aws_sso', label: 'AWS SSO' },
              ]}
            />
          </Field>
        ) : null}

        <Field
          label="Auth reference"
          hint="Role ARN, SSO profile, or the name of an environment variable holding the credential. Never the credential itself."
        >
          <input
            type="text"
            value={routingAuthRef}
            onChange={(e) => setRoutingAuthRef(e.target.value)}
            placeholder={
              provider === 'akamai_cloudlet'
                ? 'AKAMAI_EDGERC_JSON'
                : provider === 'avi_gslb'
                  ? 'AVI_API_TOKEN'
                  : 'arn:aws:iam::123456789012:role/health-checker'
            }
          />
        </Field>

        <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
          <button
            className="btn btn-secondary"
            disabled={busy}
            onClick={() => void saveRouting(false)}
          >
            Save without enabling
          </button>
          <button className="btn btn-primary" disabled={busy} onClick={() => void saveRouting(true)}>
            Save and enable
          </button>
        </div>
        <p className="text-muted" style={{ fontSize: 12, marginTop: 12, marginBottom: 0 }}>
          Enabling validates the config by building the adapter and resolving the credential
          reference — a provider that cannot authenticate is rejected here rather than during an
          incident.
        </p>
      </Card>
    </>
  )

  function saveRouting(enable: boolean) {
    let config: Record<string, unknown> = {}
    if (routingCfg.trim()) {
      try {
        config = JSON.parse(routingCfg) as Record<string, unknown>
      } catch {
        setError(new Error('Provider config must be valid JSON'))
        return Promise.resolve()
      }
    }
    return run(() =>
      api.upsertRouting(provider, {
        enabled: enable,
        config,
        authMethod: routingAuthMethod,
        authRef: routingAuthRef,
      }),
    )
  }
}

function configHint(p: RoutingProviderId): string {
  switch (p) {
    case 'aws_native':
      return 'Requires hostedZoneId, recordName, and regionTargets (region id → ALB/NLB hostname).'
    case 'cloudfront':
      return 'Requires distributionId, originGroupId, and regionOrigins (region id → origin id). CloudFront origin groups are primary/secondary, so weights collapse to a 100/0 split.'
    case 'avi_gslb':
      return 'Requires controllerUrl, gslbServiceName, username, and regionMembers (region id → pool member IP or FQDN).'
    case 'akamai_cloudlet':
      return 'Requires policyId, network (staging|production), and regionOrigins (region id → Cloudlet origin id).'
    case 'aws_arc':
      return 'Requires clusterEndpoints (all five, in retry order) and routingControls (region id → routing control ARN). Routing controls are On/Off, so weights collapse to in-rotation or out — pair with a weighted provider for proportional shifts.'
  }
}

function configPlaceholder(p: RoutingProviderId): string {
  switch (p) {
    case 'aws_native':
      return '{\n  "hostedZoneId": "Z123456ABCDEF",\n  "recordName": "app.example.com",\n  "regionTargets": {\n    "us-east-1": "east-alb.example.com",\n    "us-west-2": "west-alb.example.com"\n  }\n}'
    case 'cloudfront':
      return '{\n  "distributionId": "E1ABCDEF",\n  "originGroupId": "app-origins",\n  "regionOrigins": {\n    "us-east-1": "east-origin",\n    "us-west-2": "west-origin"\n  }\n}'
    case 'avi_gslb':
      return '{\n  "controllerUrl": "https://avi.example.com",\n  "gslbServiceName": "app-gslb",\n  "username": "svc-health-checker",\n  "regionMembers": {\n    "us-east-1": "10.0.1.10",\n    "us-west-2": "10.1.1.10"\n  }\n}'
    case 'akamai_cloudlet':
      return '{\n  "policyId": "12345",\n  "network": "production",\n  "regionOrigins": {\n    "us-east-1": "east_origin",\n    "us-west-2": "west_origin"\n  }\n}'
    case 'aws_arc':
      return '{\n  "clusterEndpoints": [\n    {"endpoint": "https://aaaa.route53-recovery-cluster.us-east-1.amazonaws.com/v1", "region": "us-east-1"},\n    {"endpoint": "https://bbbb.route53-recovery-cluster.us-west-2.amazonaws.com/v1", "region": "us-west-2"},\n    {"endpoint": "https://cccc.route53-recovery-cluster.eu-west-1.amazonaws.com/v1", "region": "eu-west-1"},\n    {"endpoint": "https://dddd.route53-recovery-cluster.ap-northeast-1.amazonaws.com/v1", "region": "ap-northeast-1"},\n    {"endpoint": "https://eeee.route53-recovery-cluster.ap-southeast-2.amazonaws.com/v1", "region": "ap-southeast-2"}\n  ],\n  "routingControls": {\n    "us-east-1": "arn:aws:route53-recovery-control::111122223333:controlpanel/abc/routingcontrol/east",\n    "us-west-2": "arn:aws:route53-recovery-control::111122223333:controlpanel/abc/routingcontrol/west"\n  }\n}'
  }
}
