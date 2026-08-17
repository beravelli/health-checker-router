import type {
  AuditEntry,
  FailOpenState,
  HealthSnapshot,
  Incident,
  Me,
  Region,
  RoutingEndpointsResponse,
  RoutingProviderId,
  Rollout,
  Service,
  WeightsState,
} from './types'

/**
 * ApiError carries the server's status and, for 403s, the LDAP group the
 * caller is missing — which is what lets the Admin tab render an accurate
 * access-restricted message instead of hardcoding a group name.
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly requiredGroup?: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }

  get isForbidden() {
    return this.status === 403
  }
  get isUnauthorized() {
    return this.status === 401
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    // The session lives in an HttpOnly cookie, so every call must send it.
    credentials: 'same-origin',
    headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  })

  if (res.status === 204) return undefined as T

  const text = await res.text()
  const body = text ? safeParse(text) : null

  if (!res.ok) {
    const fields = (body ?? {}) as Record<string, unknown>
    const message =
      typeof fields.error === 'string' && fields.error
        ? fields.error
        : `Request failed with ${res.status}`
    const requiredGroup =
      typeof fields.requiredGroup === 'string' ? fields.requiredGroup : undefined
    throw new ApiError(res.status, message, requiredGroup)
  }

  return body as T
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

export const api = {
  me: () => request<Me>('/api/me'),

  login: (username: string, password: string) =>
    request<{ user: Me['user']; isAdmin: boolean; adminGroup: string }>('/api/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),

  logout: () => request<void>('/api/logout', { method: 'POST' }),

  health: () => request<HealthSnapshot>('/api/health'),
  regions: () => request<Region[] | null>('/api/regions'),
  services: () => request<Service[] | null>('/api/services'),
  rollouts: () => request<Rollout[] | null>('/api/rollouts'),
  failOpen: () => request<FailOpenState>('/api/failopen'),
  weights: () => request<WeightsState>('/api/lb/weights'),
  routingEndpoints: () => request<RoutingEndpointsResponse>('/api/routing/endpoints'),
  audit: () => request<AuditEntry[] | null>('/api/admin/audit'),

  incidents: (severity: string) =>
    request<Incident[] | null>(
      `/api/incidents${severity && severity !== 'all' ? `?severity=${encodeURIComponent(severity)}` : ''}`,
    ),

  setWeights: (weights: Record<string, number>) =>
    request<WeightsState>('/api/lb/weights', {
      method: 'POST',
      body: JSON.stringify({ weights }),
    }),

  resetWeights: () => request<WeightsState>('/api/lb/reset', { method: 'POST' }),

  failover: (region: string) =>
    request<{ region: string }>('/api/lb/failover', {
      method: 'POST',
      body: JSON.stringify({ region }),
    }),

  pauseRollout: (id: string) => request<unknown>(`/api/rollouts/${id}/pause`, { method: 'POST' }),
  resumeRollout: (id: string) => request<unknown>(`/api/rollouts/${id}/resume`, { method: 'POST' }),
  rollbackRollout: (id: string) =>
    request<unknown>(`/api/rollouts/${id}/rollback`, { method: 'POST' }),

  createService: (body: unknown) =>
    request<Service>('/api/admin/services', { method: 'POST', body: JSON.stringify(body) }),

  updateServiceRole: (id: string, role: string) =>
    request<unknown>(`/api/admin/services/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ role }),
    }),

  deleteService: (id: string) =>
    request<void>(`/api/admin/services/${id}`, { method: 'DELETE' }),

  upsertRegion: (body: unknown) =>
    request<Region>('/api/admin/regions', { method: 'POST', body: JSON.stringify(body) }),

  deleteRegion: (id: string) => request<void>(`/api/admin/regions/${id}`, { method: 'DELETE' }),

  upsertRouting: (provider: RoutingProviderId, body: unknown) =>
    request<unknown>(`/api/admin/routing/${provider}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
}
