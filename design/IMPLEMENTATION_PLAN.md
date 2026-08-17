# Implementation Plan: DB Schema + Kubernetes Deployment

Companion to README.md. This covers persistence and infra so Claude Code can scaffold a deployable prototype, not just the app code.

## Database schema (PostgreSQL)

```sql
CREATE TYPE health_status AS ENUM ('healthy', 'degraded', 'critical', 'unknown');
CREATE TYPE probe_type AS ENUM ('http', 'tcp', 'aws_alb', 'aws_nlb', 'grpc', 'custom');
CREATE TYPE routing_provider AS ENUM ('aws_native', 'cloudfront', 'avi_gslb', 'akamai_cloudlet');
CREATE TYPE aws_auth_method AS ENUM ('assume_role', 'service_account', 'aws_sso', 'custom_sso_binary');
CREATE TYPE probe_role AS ENUM ('healthcheck_only', 'healthcheck_failover');

CREATE TABLE regions (
  id text PRIMARY KEY,               -- e.g. 'us-east-1'
  display_name text NOT NULL,
  created_at timestamptz DEFAULT now()
);

CREATE TABLE services (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  runtime text NOT NULL,
  role probe_role NOT NULL DEFAULT 'healthcheck_only',
  probe_type probe_type NOT NULL DEFAULT 'http',
  probe_interval_s int NOT NULL DEFAULT 5,
  probe_timeout_s int NOT NULL DEFAULT 3,
  consecutive_failures_to_trip int NOT NULL DEFAULT 3,
  consecutive_success_to_recover int NOT NULL DEFAULT 2,
  ca_cert_secret_env text,           -- env var name, not raw cert
  aws_auth_method aws_auth_method,
  aws_auth_ref text,                 -- role ARN / secret path / SSO profile / binary path
  created_at timestamptz DEFAULT now(),
  updated_at timestamptz DEFAULT now()
);

CREATE TABLE service_regions (       -- which regions a service is deployed/probed in
  service_id uuid REFERENCES services(id) ON DELETE CASCADE,
  region_id text REFERENCES regions(id),
  version text,                      -- deployed version, for skew detection
  PRIMARY KEY (service_id, region_id)
);

CREATE TABLE probe_results (         -- append-only, drives uptime trend + rolling status
  id bigserial PRIMARY KEY,
  service_id uuid REFERENCES services(id) ON DELETE CASCADE,
  region_id text REFERENCES regions(id),
  status health_status NOT NULL,
  latency_ms int,
  probed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON probe_results (service_id, region_id, probed_at DESC);

CREATE TABLE dependency_status (     -- Postgres/Redis/Kafka/Envoy edge per region
  id bigserial PRIMARY KEY,
  region_id text REFERENCES regions(id),
  dependency_name text NOT NULL,
  status health_status NOT NULL,
  checked_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE lb_weights (
  region_id text PRIMARY KEY REFERENCES regions(id),
  weight int NOT NULL CHECK (weight BETWEEN 0 AND 100),
  is_automatic boolean NOT NULL DEFAULT true,
  updated_at timestamptz DEFAULT now()
);

CREATE TABLE fail_open_events (
  id bigserial PRIMARY KEY,
  triggered_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  reason text
);

CREATE TABLE rollouts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id uuid REFERENCES services(id),
  from_version text NOT NULL,
  to_version text NOT NULL,
  status text NOT NULL DEFAULT 'in_progress',
  east_pct int NOT NULL DEFAULT 0,
  west_pct int NOT NULL DEFAULT 0,
  paused boolean NOT NULL DEFAULT false,
  created_at timestamptz DEFAULT now()
);

CREATE TABLE incidents (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  occurred_at timestamptz NOT NULL DEFAULT now(),
  severity text NOT NULL,            -- critical | warning | resolved
  region_id text REFERENCES regions(id),
  title text NOT NULL,
  description text
);

CREATE TABLE routing_endpoints (      -- one row per provider config shown in Admin
  provider routing_provider PRIMARY KEY,
  enabled boolean NOT NULL DEFAULT false,
  config jsonb NOT NULL DEFAULT '{}', -- provider-specific fields (zone ID, distribution ID, gslb service name, policy ID...)
  auth_method text,
  auth_ref text,                      -- env var / secret path reference only
  updated_at timestamptz DEFAULT now()
);

CREATE TABLE admin_audit_log (
  id bigserial PRIMARY KEY,
  actor text NOT NULL,               -- LDAP username
  action text NOT NULL,
  target text,
  at timestamptz DEFAULT now()
);
```

Notes:
- `probe_results` grows fast (one row per probe tick per target) — partition by day or roll up to a `probe_results_hourly` materialized view for the 7-day trend chart; keep raw rows for ~7 days via a cron job (`pg_cron` or a k8s CronJob).
- No secret values are ever stored in Postgres — only env var names / secret-manager paths, matching the design's "store a reference, not the secret" pattern.

## Suggested repo layout addition

```
/cmd/api            # HTTP/gRPC server
/internal/...       # per README.md's Go backend structure
/db/migrations      # golang-migrate or goose SQL migrations (the schema above, as versioned files)
/deploy/helm        # Helm chart (below)
/deploy/k8s         # raw manifests if not using Helm
```

## Kubernetes deployment

Two pieces to run: the Go API/prober service, and Postgres.

**Postgres**: don't hand-roll a StatefulSet — use [CloudNativePG](https://cloudnative-pg.io/) or the Bitnami `postgresql` Helm chart for a prototype. Gives you a ready `Cluster`/`StatefulSet` with backups and failover for free.

```yaml
# deploy/helm/values.yaml (excerpt)
api:
  image: <registry>/health-checker-api:latest
  replicas: 2
  env:
    - name: DATABASE_URL
      valueFrom: { secretKeyRef: { name: health-checker-db, key: url } }
    - name: LDAP_URL
      valueFrom: { secretKeyRef: { name: health-checker-ldap, key: url } }
  probes:
    liveness: /healthz
    readiness: /readyz
postgresql:
  enabled: true
  auth:
    database: health_checker
```

Manifest shape (`deploy/k8s/` if not using a chart):
- `api-deployment.yaml` — Deployment, 2+ replicas, `livenessProbe`/`readinessProbe` on `/healthz`/`/readyz`, resource requests/limits, `envFrom` a ConfigMap for non-secret config + individual `secretKeyRef`s for `DATABASE_URL` and any provider auth refs.
- `api-service.yaml` — ClusterIP Service, fronted by an `Ingress` (or your existing ingress controller) for the dashboard + API.
- `configmap.yaml` — non-secret config: probe defaults, LDAP admin group name, enabled routing providers.
- `secret.yaml` — **do not commit real values**; use `External Secrets Operator` or `Vault Agent Injector` to sync from your secrets engine into a k8s `Secret` at deploy time (matches the app's own "env var name, not raw secret" convention one layer up — the k8s Secret is what actually populates that env var at runtime).
- `migration-job.yaml` — a `Job` running `goose up` / `migrate` against `DATABASE_URL` on each deploy (Helm hook `pre-install,pre-upgrade`).
- `hpa.yaml` — HorizontalPodAutoscaler on the API deployment (CPU or request-rate) since probing load scales with service count.
- `networkpolicy.yaml` — restrict egress from the API pod to only the AWS/Avi/Akamai/LDAP endpoints it needs to call, plus Postgres.

## What Claude Code should actually build

1. Go module + `cmd/api` skeleton, `internal/` packages per README.md's backend structure.
2. `db/migrations` with the schema above.
3. Wire the prober → aggregator → fail-open → LB controller pipeline against Postgres instead of in-memory mocks.
4. REST/gRPC + SSE endpoints matching the UI's data needs (listed in README.md).
5. `deploy/helm` chart (or plain manifests) as above; a `Makefile`/`skaffold.yaml` for local iterate-and-deploy against a kind/minikube cluster.
6. A thin frontend (React, per README.md) that calls the real API — recreate the HTML design's screens, don't ship the HTML.

Point Claude Code at this file + README.md together as the spec; it has enough to scaffold Go services, migrations, and Helm/k8s manifests in one pass.
