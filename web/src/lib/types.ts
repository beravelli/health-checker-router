// Types mirroring the Go models in internal/models. Kept hand-written rather
// than generated so the shapes stay readable; if they drift, the Go structs
// are the source of truth.

export type Status = 'healthy' | 'degraded' | 'critical' | 'unknown'
export type Severity = 'critical' | 'warning' | 'resolved'
export type ProbeRole = 'healthcheck_only' | 'healthcheck_failover'
export type ProbeType = 'http' | 'tcp' | 'aws_alb' | 'aws_nlb' | 'grpc' | 'custom'
export type RoutingProviderId =
  | 'aws_native'
  | 'cloudfront'
  | 'avi_gslb'
  | 'akamai_cloudlet'
  | 'aws_arc'
export type AWSAuthMethod = 'assume_role' | 'service_account' | 'aws_sso' | 'custom_sso_binary'

export interface Region {
  id: string
  displayName: string
  provider: string
  createdAt: string
}

export interface ServiceRegion {
  regionId: string
  version?: string
  endpoint?: string
}

export interface Service {
  id: string
  name: string
  runtime: string
  role: ProbeRole
  probeType: ProbeType
  probeIntervalS: number
  probeTimeoutS: number
  consecutiveFailuresToTrip: number
  consecutiveSuccessToRecover: number
  caCertSecretEnv?: string
  awsAuthMethod?: AWSAuthMethod
  awsAuthRef?: string
  probeConfig: Record<string, unknown>
  regions: ServiceRegion[]
}

export interface DependencyStatus {
  regionId: string
  name: string
  status: Status
  checkedAt: string
}

export interface TrendPoint {
  day: string
  uptimePct: number
}

export interface RegionHealth {
  regionId: string
  name: string
  zone: string
  status: Status
  score: number
  trafficPct: number
  latencyMs: number
  errorRatePct: number
  replicationLagMs: number
  uptimePct: number
  version: string
  azHealthy: string
  nodes: number
  proxies: number
  lastProbe: string
  deps: DependencyStatus[] | null
  trend: TrendPoint[] | null
}

export interface ServiceHealth {
  serviceId: string
  name: string
  runtime: string
  byRegion: Record<string, Status>
  versions: Record<string, string>
  skew: boolean
  note: string
}

export interface HealthSnapshot {
  regions: RegionHealth[] | null
  services: ServiceHealth[] | null
  takenAt: string
}

export interface Rollout {
  id: string
  serviceId: string
  service: string
  fromVersion: string
  toVersion: string
  status: string
  eastPct: number
  westPct: number
  paused: boolean
  createdAt: string
}

export interface Incident {
  id: string
  occurredAt: string
  severity: Severity
  regionId: string
  title: string
  description: string
}

export interface FailOpenPolicy {
  probeIntervalSeconds: number
  failuresToTrip: number
  successToRecover: number
  quorumRequired: number
  quorumTotal: number
  holdSeconds: number
}

export interface FailOpenState {
  active: boolean
  event: { id: number; triggeredAt: string; reason: string } | null
  policy: FailOpenPolicy
}

export interface WeightsState {
  weights: Record<string, number>
  automatic: boolean
}

export interface RoutingEndpoint {
  provider: RoutingProviderId
  enabled: boolean
  config: Record<string, unknown>
  authMethod?: string
  authRef?: string
  updatedAt: string
}

export interface RoutingEndpointsResponse {
  endpoints: RoutingEndpoint[] | null
  registered: RoutingProviderId[] | null
}

export interface Me {
  authenticated: boolean
  user?: { username: string; groups: string[] }
  isAdmin?: boolean
  adminGroup: string
}

export interface AuditEntry {
  id: number
  actor: string
  action: string
  target: string
  at: string
}
