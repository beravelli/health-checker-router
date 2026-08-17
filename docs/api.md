# API reference

Base path `/api`. All responses are JSON. Authentication is a session cookie
issued by `POST /api/login`.

Errors share one shape:

```json
{ "error": "human readable message", "requiredGroup": "sre-admins" }
```

`requiredGroup` appears only on 403.

---

## Health probes (unauthenticated)

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/healthz` | Liveness. Always 200 if the process is up. |
| GET | `/readyz` | Readiness. 503 when Postgres is unreachable, so the pod leaves the Service rather than serving errors. |

---

## Session

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| POST | `/api/login` | `{username, password}` | Sets an HttpOnly session cookie |
| POST | `/api/logout` | — | Clears the cookie and invalidates cached LDAP groups |
| GET | `/api/me` | — | Returns `{authenticated, user, isAdmin, adminGroup}` |

`/api/me` returns 200 with `authenticated: false` when signed out, rather than
401, so the frontend can render a login screen without treating it as an error.

---

## Read (any authenticated user)

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/api/health` | Composite snapshot: region health, service matrix, trend |
| GET | `/api/regions` | Configured regions |
| GET | `/api/services` | Registered probe targets with their config |
| GET | `/api/incidents?severity=&limit=` | Incident timeline |
| GET | `/api/rollouts` | Rollout progress |
| GET | `/api/lb/weights` | `{weights, automatic}` |
| GET | `/api/failopen` | Fail-open state and the active policy |
| GET | `/api/routing/endpoints` | Provider configs (references only, never secrets) |
| GET | `/api/events` | SSE stream |

### `GET /api/health`

```json
{
  "regions": [{
    "regionId": "us-east-1",
    "name": "us-east-1",
    "zone": "primary write region",
    "status": "healthy",
    "score": 0.98,
    "trafficPct": 55,
    "latencyMs": 84,
    "errorRatePct": 0.12,
    "replicationLagMs": 40,
    "uptimePct": 99.98,
    "version": "v2.15.0",
    "azHealthy": "3 / 3",
    "lastProbe": "2026-08-15T09:42:00Z",
    "deps": [{"regionId": "us-east-1", "name": "Postgres", "status": "healthy"}],
    "trend": [{"day": "M", "uptimePct": 99.9}]
  }],
  "services": [{
    "serviceId": "…",
    "name": "checkout-service",
    "byRegion": {"us-east-1": "healthy", "us-west-2": "degraded"},
    "versions": {"us-east-1": "v2.15.0", "us-west-2": "v2.14.0"},
    "skew": true,
    "note": "Version skew: us-east-1 on v2.15.0 and us-west-2 on v2.14.0"
  }],
  "takenAt": "2026-08-15T09:42:03Z"
}
```

`score` is the composite: 60% failover-eligible service health, 40% region
dependency health. `status` derives from it (≥0.95 healthy, ≥0.70 degraded,
below that critical).

### `GET /api/events` (SSE)

Event types: `health`, `weights`, `failopen`, `incident`, `rollout`.

```
retry: 3000

event: health
data: {"type":"health","at":"…","data":{…}}

: keepalive
```

Keepalive comments every 25s keep the connection through proxies with idle
timeouts. Slow subscribers drop events rather than applying backpressure to the
probe loop.

---

## Mutations (admin group required)

| Method | Path | Body |
| --- | --- | --- |
| POST | `/api/lb/weights` | `{weights: {"us-east-1": 60, "us-west-2": 40}}` |
| POST | `/api/lb/reset` | — |
| POST | `/api/lb/failover` | `{region: "us-west-2"}` |
| POST | `/api/rollouts/{id}/pause` | — |
| POST | `/api/rollouts/{id}/resume` | — |
| POST | `/api/rollouts/{id}/rollback` | — |
| POST | `/api/admin/services` | See [adding-services.md](./adding-services.md) |
| PATCH | `/api/admin/services/{id}` | `{role}` |
| DELETE | `/api/admin/services/{id}` | — |
| POST | `/api/admin/regions` | `{id, displayName, provider, zoneLabel}` |
| DELETE | `/api/admin/regions/{id}` | — |
| PUT | `/api/admin/routing/{provider}` | `{enabled, config, authMethod, authRef}` |
| GET | `/api/admin/audit?limit=` | — |

Notes on a few:

**`POST /api/lb/weights`** — must sum to exactly 100. Switches to manual
control; automatic reweighting stops until `/api/lb/reset`.

**`POST /api/lb/failover`** — 100% to the named region, everything else to
zero. Also records a critical incident.

**`POST /api/rollouts/{id}/resume`** — returns **409** with the reason if the
canary region is critical or its health is inconclusive. That is a policy
decision, not a fault; the message explains what blocked it.

**`PUT /api/admin/routing/{provider}`** — with `enabled: true`, builds the
adapter to validate config and resolve credentials. Returns 400 with the
specific failure if that doesn't work.

---

## Status codes

| Code | Meaning |
| --- | --- |
| 200 / 201 / 204 | Success |
| 400 | Invalid input — message names the problem |
| 401 | Not signed in |
| 403 | Signed in, missing admin group — `requiredGroup` names it |
| 404 | No such resource |
| 409 | Refused by policy (currently: rollout resume blocked by canary health) |
| 500 | Server fault. Details are logged, not returned |
| 503 | Not ready (database unreachable) |
