// Package store is the Postgres persistence layer.
//
// Every query lives here so the domain packages stay free of SQL. The pool is
// shared; pgx handles concurrency.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/beravelli/health-checker-router/internal/models"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// Store wraps a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a connection pool against databaseURL and verifies it.
func New(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for callers that need a dedicated
// connection rather than a query.
//
// Only internal/leader uses this, because a session-scoped advisory lock must
// be held on one connection for the whole term and must not be returned to the
// pool while held. Everything else goes through the methods on Store.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping checks database reachability, for /readyz.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ---------------------------------------------------------------- regions

// ListRegions returns all configured regions, oldest first.
func (s *Store) ListRegions(ctx context.Context) ([]models.Region, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, display_name, provider, created_at FROM regions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Region
	for rows.Next() {
		var r models.Region
		if err := rows.Scan(&r.ID, &r.DisplayName, &r.Provider, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegionZoneLabels maps region id to its display kicker ("primary write region").
func (s *Store) RegionZoneLabels(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, zone_label FROM regions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		out[id] = label
	}
	return out, rows.Err()
}

// UpsertRegion creates or updates a region.
func (s *Store) UpsertRegion(ctx context.Context, r models.Region, zoneLabel string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO regions (id, display_name, provider, zone_label)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		  SET display_name = EXCLUDED.display_name,
		      provider     = EXCLUDED.provider,
		      zone_label   = EXCLUDED.zone_label`,
		r.ID, r.DisplayName, r.Provider, zoneLabel)
	return err
}

// DeleteRegion removes a region and everything scoped to it.
func (s *Store) DeleteRegion(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM regions WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --------------------------------------------------------------- services

const serviceCols = `
	s.id, s.name, s.runtime, s.role, s.probe_type, s.probe_config,
	s.probe_interval_s, s.probe_timeout_s,
	s.consecutive_failures_to_trip, s.consecutive_success_to_recover,
	COALESCE(s.ca_cert_secret_env, ''), COALESCE(s.aws_auth_method::text, ''),
	COALESCE(s.aws_auth_ref, ''), s.created_at, s.updated_at`

func scanService(row pgx.Row) (models.Service, error) {
	var (
		svc        models.Service
		cfgBytes   []byte
		authMethod string
	)
	err := row.Scan(
		&svc.ID, &svc.Name, &svc.Runtime, &svc.Role, &svc.ProbeType, &cfgBytes,
		&svc.ProbeIntervalS, &svc.ProbeTimeoutS,
		&svc.ConsecutiveFailuresToTrip, &svc.ConsecutiveSuccessToRecover,
		&svc.CACertSecretEnv, &authMethod, &svc.AWSAuthRef,
		&svc.CreatedAt, &svc.UpdatedAt,
	)
	if err != nil {
		return svc, err
	}
	svc.AWSAuthMethod = models.AWSAuthMethod(authMethod)
	svc.ProbeConfig = map[string]any{}
	if len(cfgBytes) > 0 {
		if err := json.Unmarshal(cfgBytes, &svc.ProbeConfig); err != nil {
			return svc, fmt.Errorf("service %s: decode probe_config: %w", svc.ID, err)
		}
	}
	return svc, nil
}

// ListServices returns every registered service with its region bindings.
func (s *Store) ListServices(ctx context.Context) ([]models.Service, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serviceCols+` FROM services s ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out  []models.Service
		byID = map[string]int{}
	)
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		byID[svc.ID] = len(out)
		out = append(out, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	srRows, err := s.pool.Query(ctx,
		`SELECT service_id, region_id, version, endpoint FROM service_regions`)
	if err != nil {
		return nil, err
	}
	defer srRows.Close()

	for srRows.Next() {
		var sid string
		var sr models.ServiceRegion
		if err := srRows.Scan(&sid, &sr.RegionID, &sr.Version, &sr.Endpoint); err != nil {
			return nil, err
		}
		if idx, ok := byID[sid]; ok {
			out[idx].Regions = append(out[idx].Regions, sr)
		}
	}
	return out, srRows.Err()
}

// GetService loads one service by id.
func (s *Store) GetService(ctx context.Context, id string) (models.Service, error) {
	svc, err := scanService(s.pool.QueryRow(ctx,
		`SELECT `+serviceCols+` FROM services s WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return svc, ErrNotFound
	}
	if err != nil {
		return svc, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT region_id, version, endpoint FROM service_regions WHERE service_id = $1`, id)
	if err != nil {
		return svc, err
	}
	defer rows.Close()
	for rows.Next() {
		var sr models.ServiceRegion
		if err := rows.Scan(&sr.RegionID, &sr.Version, &sr.Endpoint); err != nil {
			return svc, err
		}
		svc.Regions = append(svc.Regions, sr)
	}
	return svc, rows.Err()
}

// CreateService registers a new probe target and its region bindings.
func (s *Store) CreateService(ctx context.Context, svc models.Service) (models.Service, error) {
	cfg, err := json.Marshal(svc.ProbeConfig)
	if err != nil {
		return svc, fmt.Errorf("encode probe_config: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return svc, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO services (
			name, runtime, role, probe_type, probe_config,
			probe_interval_s, probe_timeout_s,
			consecutive_failures_to_trip, consecutive_success_to_recover,
			ca_cert_secret_env, aws_auth_method, aws_auth_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,
		        NULLIF($10,''), NULLIF($11,'')::aws_auth_method, NULLIF($12,''))
		RETURNING id, created_at, updated_at`,
		svc.Name, svc.Runtime, svc.Role, svc.ProbeType, cfg,
		svc.ProbeIntervalS, svc.ProbeTimeoutS,
		svc.ConsecutiveFailuresToTrip, svc.ConsecutiveSuccessToRecover,
		svc.CACertSecretEnv, string(svc.AWSAuthMethod), svc.AWSAuthRef,
	).Scan(&svc.ID, &svc.CreatedAt, &svc.UpdatedAt)
	if err != nil {
		return svc, err
	}

	for _, r := range svc.Regions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_regions (service_id, region_id, version, endpoint)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (service_id, region_id) DO UPDATE
			  SET version = EXCLUDED.version, endpoint = EXCLUDED.endpoint`,
			svc.ID, r.RegionID, r.Version, r.Endpoint); err != nil {
			return svc, err
		}
	}
	return svc, tx.Commit(ctx)
}

// UpdateServiceRole flips a service between monitor-only and failover-capable.
func (s *Store) UpdateServiceRole(ctx context.Context, id string, role models.ProbeRole) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE services SET role = $2, updated_at = now() WHERE id = $1`, id, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteService removes a service and its probe history.
//
// probe_results is partitioned and deliberately carries no foreign key to
// services (a FK from a partitioned table adds a per-row check to the hottest
// write path in the system), so its rows are removed explicitly here rather
// than by a cascade.
func (s *Store) DeleteService(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `DELETE FROM services WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM probe_results WHERE service_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------- probe results

// RecordProbe appends one probe observation.
func (s *Store) RecordProbe(ctx context.Context, r models.ProbeResult) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO probe_results (service_id, region_id, status, latency_ms, detail, probed_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		r.ServiceID, r.RegionID, r.Status, r.LatencyMs, r.Detail, r.ProbedAt)
	return err
}

// LatestProbeByServiceRegion returns the most recent probe per (service, region).
func (s *Store) LatestProbeByServiceRegion(ctx context.Context) (map[string]models.ProbeResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (service_id, region_id)
		       service_id, region_id, status, COALESCE(latency_ms, 0), detail, probed_at
		FROM probe_results
		WHERE probed_at > now() - interval '1 hour'
		ORDER BY service_id, region_id, probed_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]models.ProbeResult{}
	for rows.Next() {
		var p models.ProbeResult
		if err := rows.Scan(&p.ServiceID, &p.RegionID, &p.Status, &p.LatencyMs, &p.Detail, &p.ProbedAt); err != nil {
			return nil, err
		}
		out[p.ServiceID+"|"+p.RegionID] = p
	}
	return out, rows.Err()
}

// UptimeTrend returns per-region daily uptime percentages for the last days
// days, ordered oldest first. Reads the hourly rollup, not raw probe rows.
func (s *Store) UptimeTrend(ctx context.Context, days int) (map[string][]models.TrendPoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT region_id,
		       date_trunc('day', bucket) AS day,
		       CASE WHEN sum(total) = 0 THEN 100
		            ELSE round(100.0 * sum(healthy) / sum(total), 2) END AS uptime_pct
		FROM probe_results_hourly
		WHERE bucket >= date_trunc('day', now()) - make_interval(days => $1)
		GROUP BY 1, 2
		ORDER BY 1, 2`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]models.TrendPoint{}
	for rows.Next() {
		var region string
		var day time.Time
		var pct float64
		if err := rows.Scan(&region, &day, &pct); err != nil {
			return nil, err
		}
		out[region] = append(out[region], models.TrendPoint{
			Day:       day.Format("Mon")[:1],
			UptimePct: pct,
		})
	}
	return out, rows.Err()
}

// Uptime30d returns the 30-day uptime percentage per region.
func (s *Store) Uptime30d(ctx context.Context) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT region_id,
		       CASE WHEN sum(total) = 0 THEN 100
		            ELSE round(100.0 * sum(healthy) / sum(total), 2) END
		FROM probe_results_hourly
		WHERE bucket >= now() - interval '30 days'
		GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var region string
		var pct float64
		if err := rows.Scan(&region, &pct); err != nil {
			return nil, err
		}
		out[region] = pct
	}
	return out, rows.Err()
}

// RefreshRollup recomputes the hourly rollup backing the trend chart.
func (s *Store) RefreshRollup(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY probe_results_hourly`)
	return err
}

// EnsurePartitions rolls probe_results partitions forward and prunes old ones.
func (s *Store) EnsurePartitions(ctx context.Context, retainDays int) error {
	_, err := s.pool.Exec(ctx, `SELECT ensure_probe_partitions($1)`, retainDays)
	return err
}

// ----------------------------------------------------------- dependencies

// RecordDependency appends a dependency observation.
func (s *Store) RecordDependency(ctx context.Context, d models.DependencyStatus) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dependency_status (region_id, dependency_name, status, checked_at)
		VALUES ($1, $2, $3, $4)`, d.RegionID, d.Name, d.Status, d.CheckedAt)
	return err
}

// LatestDependencies returns the newest status per (region, dependency).
func (s *Store) LatestDependencies(ctx context.Context) (map[string][]models.DependencyStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (region_id, dependency_name)
		       region_id, dependency_name, status, checked_at
		FROM dependency_status
		ORDER BY region_id, dependency_name, checked_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]models.DependencyStatus{}
	for rows.Next() {
		var d models.DependencyStatus
		if err := rows.Scan(&d.RegionID, &d.Name, &d.Status, &d.CheckedAt); err != nil {
			return nil, err
		}
		out[d.RegionID] = append(out[d.RegionID], d)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- lb weights

// GetWeights returns the current weight per region.
func (s *Store) GetWeights(ctx context.Context) ([]models.LBWeight, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT region_id, weight, is_automatic, updated_at FROM lb_weights ORDER BY region_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.LBWeight
	for rows.Next() {
		var w models.LBWeight
		if err := rows.Scan(&w.RegionID, &w.Weight, &w.IsAutomatic, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWeights replaces all region weights atomically. The caller is responsible
// for ensuring they sum to 100.
func (s *Store) SetWeights(ctx context.Context, weights map[string]int, automatic bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for region, w := range weights {
		if _, err := tx.Exec(ctx, `
			INSERT INTO lb_weights (region_id, weight, is_automatic, updated_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (region_id) DO UPDATE
			  SET weight = EXCLUDED.weight,
			      is_automatic = EXCLUDED.is_automatic,
			      updated_at = now()`, region, w, automatic); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// --------------------------------------------------------------- fail-open

// ActiveFailOpen returns the currently open fail-open window, if any.
func (s *Store) ActiveFailOpen(ctx context.Context) (*models.FailOpenEvent, error) {
	var e models.FailOpenEvent
	err := s.pool.QueryRow(ctx, `
		SELECT id, triggered_at, resolved_at, reason
		FROM fail_open_events WHERE resolved_at IS NULL
		ORDER BY triggered_at DESC LIMIT 1`).
		Scan(&e.ID, &e.TriggeredAt, &e.ResolvedAt, &e.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// OpenFailOpen starts a fail-open window, or returns the existing one.
func (s *Store) OpenFailOpen(ctx context.Context, reason string) (*models.FailOpenEvent, error) {
	if existing, err := s.ActiveFailOpen(ctx); err != nil || existing != nil {
		return existing, err
	}
	var e models.FailOpenEvent
	err := s.pool.QueryRow(ctx, `
		INSERT INTO fail_open_events (reason) VALUES ($1)
		RETURNING id, triggered_at, resolved_at, reason`, reason).
		Scan(&e.ID, &e.TriggeredAt, &e.ResolvedAt, &e.Reason)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ResolveFailOpen closes the open fail-open window.
func (s *Store) ResolveFailOpen(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE fail_open_events SET resolved_at = now() WHERE resolved_at IS NULL`)
	return err
}

// ---------------------------------------------------------------- rollouts

// ListRollouts returns rollouts newest first.
func (s *Store) ListRollouts(ctx context.Context) ([]models.Rollout, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, COALESCE(r.service_id::text, ''), COALESCE(s.name, ''),
		       r.from_version, r.to_version, r.status, r.east_pct, r.west_pct,
		       r.paused, r.created_at
		FROM rollouts r
		LEFT JOIN services s ON s.id = r.service_id
		ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Rollout
	for rows.Next() {
		var r models.Rollout
		if err := rows.Scan(&r.ID, &r.ServiceID, &r.Service, &r.FromVersion, &r.ToVersion,
			&r.Status, &r.EastPct, &r.WestPct, &r.Paused, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRolloutPaused pauses or resumes a rollout.
func (s *Store) SetRolloutPaused(ctx context.Context, id string, paused bool) error {
	status := "in_progress"
	if paused {
		status = "paused"
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE rollouts SET paused = $2, status = $3 WHERE id = $1 AND status <> 'done'`,
		id, paused, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RollbackRollout zeroes the secondary region's progress and pauses.
func (s *Store) RollbackRollout(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE rollouts SET west_pct = 0, paused = true, status = 'paused' WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --------------------------------------------------------------- incidents

// ListIncidents returns incidents, optionally filtered by severity.
func (s *Store) ListIncidents(ctx context.Context, severity string, limit int) ([]models.Incident, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, occurred_at, severity, COALESCE(region_id, ''), title, description
		FROM incidents
		WHERE ($1 = '' OR $1 = 'all' OR severity = $1)
		ORDER BY occurred_at DESC
		LIMIT $2`, severity, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Incident
	for rows.Next() {
		var i models.Incident
		if err := rows.Scan(&i.ID, &i.OccurredAt, &i.Severity, &i.RegionID, &i.Title, &i.Description); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// CreateIncident records a new incident.
func (s *Store) CreateIncident(ctx context.Context, i models.Incident) (models.Incident, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO incidents (severity, region_id, title, description)
		VALUES ($1, NULLIF($2,''), $3, $4)
		RETURNING id, occurred_at`,
		i.Severity, i.RegionID, i.Title, i.Description).
		Scan(&i.ID, &i.OccurredAt)
	return i, err
}

// RecentIncidentExists reports whether an incident with the same title was
// recorded within the window. Used to suppress duplicate pages.
func (s *Store) RecentIncidentExists(ctx context.Context, title string, within time.Duration) (bool, error) {
	// Passed as seconds rather than a Go duration string: "15m0s" is not a
	// valid Postgres interval literal.
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM incidents
			WHERE title = $1
			  AND occurred_at > now() - make_interval(secs => $2))`,
		title, within.Seconds()).Scan(&exists)
	return exists, err
}

// -------------------------------------------------------- routing endpoints

// ListRoutingEndpoints returns every provider config.
func (s *Store) ListRoutingEndpoints(ctx context.Context) ([]models.RoutingEndpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider, enabled, config, COALESCE(auth_method, ''), COALESCE(auth_ref, ''), updated_at
		FROM routing_endpoints ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.RoutingEndpoint
	for rows.Next() {
		var (
			ep       models.RoutingEndpoint
			cfgBytes []byte
		)
		if err := rows.Scan(&ep.Provider, &ep.Enabled, &cfgBytes, &ep.AuthMethod, &ep.AuthRef, &ep.UpdatedAt); err != nil {
			return nil, err
		}
		ep.Config = map[string]any{}
		if len(cfgBytes) > 0 {
			if err := json.Unmarshal(cfgBytes, &ep.Config); err != nil {
				return nil, fmt.Errorf("provider %s: decode config: %w", ep.Provider, err)
			}
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// UpsertRoutingEndpoint saves a provider config. Enabling one provider disables
// the others, since exactly one traffic manager fronts the app at a time.
func (s *Store) UpsertRoutingEndpoint(ctx context.Context, ep models.RoutingEndpoint) error {
	cfg, err := json.Marshal(ep.Config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO routing_endpoints (provider, enabled, config, auth_method, auth_ref, updated_at)
		VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), now())
		ON CONFLICT (provider) DO UPDATE
		  SET enabled     = EXCLUDED.enabled,
		      config      = EXCLUDED.config,
		      auth_method = EXCLUDED.auth_method,
		      auth_ref    = EXCLUDED.auth_ref,
		      updated_at  = now()`,
		ep.Provider, ep.Enabled, cfg, ep.AuthMethod, ep.AuthRef); err != nil {
		return err
	}

	if ep.Enabled {
		if _, err := tx.Exec(ctx,
			`UPDATE routing_endpoints SET enabled = false, updated_at = now() WHERE provider <> $1`,
			ep.Provider); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ActiveRoutingEndpoint returns the enabled provider, or ErrNotFound.
func (s *Store) ActiveRoutingEndpoint(ctx context.Context) (models.RoutingEndpoint, error) {
	var (
		ep       models.RoutingEndpoint
		cfgBytes []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT provider, enabled, config, COALESCE(auth_method, ''), COALESCE(auth_ref, ''), updated_at
		FROM routing_endpoints WHERE enabled ORDER BY updated_at DESC LIMIT 1`).
		Scan(&ep.Provider, &ep.Enabled, &cfgBytes, &ep.AuthMethod, &ep.AuthRef, &ep.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ep, ErrNotFound
	}
	if err != nil {
		return ep, err
	}
	ep.Config = map[string]any{}
	if len(cfgBytes) > 0 {
		if err := json.Unmarshal(cfgBytes, &ep.Config); err != nil {
			return ep, err
		}
	}
	return ep, nil
}

// ------------------------------------------------------------------ audit

// Audit appends an admin action to the audit log.
func (s *Store) Audit(ctx context.Context, actor, action, target string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO admin_audit_log (actor, action, target, detail) VALUES ($1,$2,$3,$4)`,
		actor, action, target, d)
	return err
}

// ListAudit returns the most recent audit entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]models.AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, actor, action, target, at FROM admin_audit_log ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.AuditEntry
	for rows.Next() {
		var e models.AuditEntry
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
