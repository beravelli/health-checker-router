-- +goose Up
-- +goose StatementBegin

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TYPE health_status    AS ENUM ('healthy', 'degraded', 'critical', 'unknown');
CREATE TYPE probe_type       AS ENUM ('http', 'tcp', 'aws_alb', 'aws_nlb', 'grpc', 'custom');
CREATE TYPE routing_provider AS ENUM ('aws_native', 'cloudfront', 'avi_gslb', 'akamai_cloudlet');
CREATE TYPE aws_auth_method  AS ENUM ('assume_role', 'service_account', 'aws_sso', 'custom_sso_binary');
CREATE TYPE probe_role       AS ENUM ('healthcheck_only', 'healthcheck_failover');

CREATE TABLE regions (
  id           text PRIMARY KEY,              -- e.g. 'us-east-1'
  display_name text NOT NULL,
  provider     text NOT NULL DEFAULT 'AWS',
  zone_label   text NOT NULL DEFAULT '',      -- 'primary write region'
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE services (
  id                             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name                           text NOT NULL UNIQUE,
  runtime                        text NOT NULL DEFAULT '',
  role                           probe_role NOT NULL DEFAULT 'healthcheck_only',
  probe_type                     probe_type NOT NULL DEFAULT 'http',
  probe_config                   jsonb NOT NULL DEFAULT '{}',
  probe_interval_s               int  NOT NULL DEFAULT 5  CHECK (probe_interval_s  BETWEEN 1 AND 3600),
  probe_timeout_s                int  NOT NULL DEFAULT 3  CHECK (probe_timeout_s   BETWEEN 1 AND 300),
  consecutive_failures_to_trip   int  NOT NULL DEFAULT 3  CHECK (consecutive_failures_to_trip   >= 1),
  consecutive_success_to_recover int  NOT NULL DEFAULT 2  CHECK (consecutive_success_to_recover >= 1),
  ca_cert_secret_env             text,        -- env var NAME, never a cert body
  aws_auth_method                aws_auth_method,
  aws_auth_ref                   text,        -- role ARN / secret path / SSO profile / binary path
  created_at                     timestamptz NOT NULL DEFAULT now(),
  updated_at                     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT timeout_below_interval CHECK (probe_timeout_s <= probe_interval_s)
);

CREATE TABLE service_regions (
  service_id uuid REFERENCES services(id) ON DELETE CASCADE,
  region_id  text REFERENCES regions(id)  ON DELETE CASCADE,
  version    text NOT NULL DEFAULT '',
  endpoint   text NOT NULL DEFAULT '',
  PRIMARY KEY (service_id, region_id)
);

-- Append-only. Drives the rolling status and the 7-day uptime trend.
-- Partitioned by day so retention is a DETACH, not a bulk DELETE.
CREATE TABLE probe_results (
  id         bigserial,
  service_id uuid NOT NULL,
  region_id  text NOT NULL,
  status     health_status NOT NULL,
  latency_ms int,
  detail     text NOT NULL DEFAULT '',
  probed_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id, probed_at)
) PARTITION BY RANGE (probed_at);

CREATE INDEX probe_results_lookup ON probe_results (service_id, region_id, probed_at DESC);

-- Bootstrap partitions. ensure_probe_partitions() below keeps them rolling.
CREATE TABLE probe_results_default PARTITION OF probe_results DEFAULT;

CREATE TABLE dependency_status (
  id              bigserial PRIMARY KEY,
  region_id       text NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  dependency_name text NOT NULL,
  status          health_status NOT NULL,
  checked_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dependency_status_latest ON dependency_status (region_id, dependency_name, checked_at DESC);

CREATE TABLE lb_weights (
  region_id    text PRIMARY KEY REFERENCES regions(id) ON DELETE CASCADE,
  weight       int NOT NULL CHECK (weight BETWEEN 0 AND 100),
  is_automatic boolean NOT NULL DEFAULT true,
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE fail_open_events (
  id           bigserial PRIMARY KEY,
  triggered_at timestamptz NOT NULL DEFAULT now(),
  resolved_at  timestamptz,
  reason       text NOT NULL DEFAULT ''
);
-- At most one open fail-open window at a time.
CREATE UNIQUE INDEX fail_open_single_active ON fail_open_events ((resolved_at IS NULL)) WHERE resolved_at IS NULL;

CREATE TABLE rollouts (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id   uuid REFERENCES services(id) ON DELETE CASCADE,
  from_version text NOT NULL,
  to_version   text NOT NULL,
  status       text NOT NULL DEFAULT 'in_progress',
  east_pct     int  NOT NULL DEFAULT 0 CHECK (east_pct BETWEEN 0 AND 100),
  west_pct     int  NOT NULL DEFAULT 0 CHECK (west_pct BETWEEN 0 AND 100),
  paused       boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE incidents (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  occurred_at timestamptz NOT NULL DEFAULT now(),
  severity    text NOT NULL CHECK (severity IN ('critical', 'warning', 'resolved')),
  region_id   text REFERENCES regions(id) ON DELETE SET NULL,
  title       text NOT NULL,
  description text NOT NULL DEFAULT ''
);
CREATE INDEX incidents_recent ON incidents (occurred_at DESC);

CREATE TABLE routing_endpoints (
  provider    routing_provider PRIMARY KEY,
  enabled     boolean NOT NULL DEFAULT false,
  config      jsonb   NOT NULL DEFAULT '{}',
  auth_method text,
  auth_ref    text,                      -- env var name / secret path only
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE admin_audit_log (
  id     bigserial PRIMARY KEY,
  actor  text NOT NULL,                  -- LDAP username
  action text NOT NULL,
  target text NOT NULL DEFAULT '',
  detail jsonb NOT NULL DEFAULT '{}',
  at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX admin_audit_recent ON admin_audit_log (at DESC);

-- +goose StatementEnd

-- +goose StatementBegin
-- Creates tomorrow's partition and drops partitions older than the retention
-- window. Call daily from a k8s CronJob or pg_cron.
CREATE OR REPLACE FUNCTION ensure_probe_partitions(retain_days int DEFAULT 7)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
  d          date;
  part_name  text;
  cutoff     date := (current_date - retain_days);
  r          record;
BEGIN
  FOR i IN 0..1 LOOP
    d := current_date + i;
    part_name := format('probe_results_%s', to_char(d, 'YYYYMMDD'));
    IF to_regclass(part_name) IS NULL THEN
      EXECUTE format(
        'CREATE TABLE %I PARTITION OF probe_results FOR VALUES FROM (%L) TO (%L)',
        part_name, d, d + 1);
    END IF;
  END LOOP;

  FOR r IN
    SELECT c.relname
    FROM pg_class c
    JOIN pg_inherits i ON i.inhrelid = c.oid
    JOIN pg_class p ON p.oid = i.inhparent
    WHERE p.relname = 'probe_results'
      AND c.relname ~ '^probe_results_[0-9]{8}$'
      AND to_date(right(c.relname, 8), 'YYYYMMDD') < cutoff
  LOOP
    EXECUTE format('DROP TABLE IF EXISTS %I', r.relname);
  END LOOP;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
SELECT ensure_probe_partitions();
-- +goose StatementEnd

-- +goose StatementBegin
-- Hourly rollup backing the 7-day trend chart, so the chart never scans raw
-- probe rows. Refresh from the same CronJob that rolls partitions.
CREATE MATERIALIZED VIEW probe_results_hourly AS
SELECT
  service_id,
  region_id,
  date_trunc('hour', probed_at)                                   AS bucket,
  count(*)                                                        AS total,
  count(*) FILTER (WHERE status = 'healthy')                      AS healthy,
  count(*) FILTER (WHERE status = 'unknown')                      AS unknown,
  avg(latency_ms) FILTER (WHERE latency_ms IS NOT NULL)           AS avg_latency_ms
FROM probe_results
GROUP BY 1, 2, 3;

CREATE UNIQUE INDEX probe_results_hourly_key
  ON probe_results_hourly (service_id, region_id, bucket);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP MATERIALIZED VIEW IF EXISTS probe_results_hourly;
DROP FUNCTION IF EXISTS ensure_probe_partitions(int);
DROP TABLE IF EXISTS admin_audit_log, routing_endpoints, incidents, rollouts,
  fail_open_events, lb_weights, dependency_status, probe_results,
  service_regions, services, regions CASCADE;
DROP TYPE IF EXISTS probe_role, aws_auth_method, routing_provider, probe_type, health_status;
-- +goose StatementEnd
