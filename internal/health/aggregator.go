// Package health turns raw per-target probe results into the composite
// region and service scores the dashboard renders.
//
// The central design choice is that health is *weighted, not binary*. A region
// with one degraded dependency is not "down" — it is a region whose score has
// dropped, and whose LB weight should drop proportionally. Collapsing that to
// up/down is what produces false full-outage signals.
package health

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/store"
)

// Weights control how much each input contributes to a region's score.
type Weights struct {
	// Services is the share of the score driven by failover-eligible services.
	Services float64
	// Dependencies is the share driven by region dependencies (Postgres,
	// Redis, Kafka, Envoy edge).
	Dependencies float64
}

// DefaultWeights splits the score 60/40 between services and dependencies.
// Dependencies are weighted heavily because a degraded datastore affects every
// service in the region, whether or not their own probes have noticed yet.
func DefaultWeights() Weights { return Weights{Services: 0.6, Dependencies: 0.4} }

// Thresholds map a composite score onto a status.
type Thresholds struct {
	// At or above Healthy the region is healthy.
	Healthy float64
	// At or above Degraded (but below Healthy) the region is degraded.
	// Below Degraded it is critical.
	Degraded float64
}

// DefaultThresholds treats >=0.95 as healthy and >=0.70 as degraded.
func DefaultThresholds() Thresholds { return Thresholds{Healthy: 0.95, Degraded: 0.70} }

// StatusProvider exposes the prober's debounced per-target status.
type StatusProvider interface {
	Status(serviceID, regionID string) (models.Status, bool)
}

// Aggregator composes region and service health.
type Aggregator struct {
	store      *store.Store
	prober     StatusProvider
	weights    Weights
	thresholds Thresholds
}

// New returns an Aggregator with default weighting.
func New(s *store.Store, p StatusProvider) *Aggregator {
	return &Aggregator{
		store:      s,
		prober:     p,
		weights:    DefaultWeights(),
		thresholds: DefaultThresholds(),
	}
}

// Snapshot is everything the Overview and Services tabs need in one read.
type Snapshot struct {
	Regions  []models.RegionHealth  `json:"regions"`
	Services []models.ServiceHealth `json:"services"`
	TakenAt  time.Time              `json:"takenAt"`
}

// Compute builds a full health snapshot from current state.
func (a *Aggregator) Compute(ctx context.Context) (Snapshot, error) {
	var snap Snapshot
	snap.TakenAt = time.Now().UTC()

	regions, err := a.store.ListRegions(ctx)
	if err != nil {
		return snap, fmt.Errorf("list regions: %w", err)
	}
	services, err := a.store.ListServices(ctx)
	if err != nil {
		return snap, fmt.Errorf("list services: %w", err)
	}
	deps, err := a.store.LatestDependencies(ctx)
	if err != nil {
		return snap, fmt.Errorf("latest dependencies: %w", err)
	}
	latest, err := a.store.LatestProbeByServiceRegion(ctx)
	if err != nil {
		return snap, fmt.Errorf("latest probes: %w", err)
	}
	weights, err := a.store.GetWeights(ctx)
	if err != nil {
		return snap, fmt.Errorf("get weights: %w", err)
	}
	trend, err := a.store.UptimeTrend(ctx, 7)
	if err != nil {
		return snap, fmt.Errorf("uptime trend: %w", err)
	}
	uptime30, err := a.store.Uptime30d(ctx)
	if err != nil {
		return snap, fmt.Errorf("30d uptime: %w", err)
	}
	zones, err := a.store.RegionZoneLabels(ctx)
	if err != nil {
		return snap, fmt.Errorf("zone labels: %w", err)
	}

	weightByRegion := map[string]int{}
	for _, w := range weights {
		weightByRegion[w.RegionID] = w.Weight
	}

	for _, region := range regions {
		rh := models.RegionHealth{
			RegionID:     region.ID,
			Name:         region.DisplayName,
			Zone:         zones[region.ID],
			TrafficPct:   weightByRegion[region.ID],
			Dependencies: deps[region.ID],
			Trend:        trend[region.ID],
			UptimePct30d: round2(orFloat(uptime30[region.ID], 100)),
		}

		rh.Score = a.regionScore(region.ID, services, deps[region.ID])
		rh.Status = a.classify(rh.Score)
		rh.LatencyMs, rh.ErrorRatePct, rh.LastProbe = regionMetrics(region.ID, services, latest)
		rh.ReplicationLagMs = replicationLag(region.ID, latest)
		rh.Version = dominantVersion(region.ID, services)
		rh.AZHealthy = azSummary(deps[region.ID])

		snap.Regions = append(snap.Regions, rh)
	}

	snap.Services = a.serviceMatrix(services, regions)
	return snap, nil
}

// regionScore combines failover-eligible service health with dependency health.
//
// Monitor-only services are excluded on purpose: registering a service for
// observation must never be able to move production traffic.
func (a *Aggregator) regionScore(regionID string, services []models.Service, deps []models.DependencyStatus) float64 {
	svcScore, svcCount := 1.0, 0
	var svcTotal float64
	for _, svc := range services {
		if svc.Role != models.RoleHealthcheckFailover || !servesRegion(svc, regionID) {
			continue
		}
		st, ok := a.prober.Status(svc.ID, regionID)
		if !ok || st == models.StatusUnknown {
			// Not yet observed, or inconclusive: excluded rather than counted
			// as healthy or as failed. Fail-open handles the unknown case.
			continue
		}
		svcTotal += st.Score()
		svcCount++
	}
	if svcCount > 0 {
		svcScore = svcTotal / float64(svcCount)
	}

	depScore, depCount := 1.0, 0
	var depTotal float64
	for _, d := range deps {
		if d.Status == models.StatusUnknown {
			continue
		}
		depTotal += d.Status.Score()
		depCount++
	}
	if depCount > 0 {
		depScore = depTotal / float64(depCount)
	}

	// If one side has no observations at all, let the other carry the score
	// rather than diluting it against an assumed-perfect half.
	switch {
	case svcCount == 0 && depCount == 0:
		return 1.0
	case svcCount == 0:
		return depScore
	case depCount == 0:
		return svcScore
	}
	return a.weights.Services*svcScore + a.weights.Dependencies*depScore
}

func (a *Aggregator) classify(score float64) models.Status {
	switch {
	case score >= a.thresholds.Healthy:
		return models.StatusHealthy
	case score >= a.thresholds.Degraded:
		return models.StatusDegraded
	default:
		return models.StatusCritical
	}
}

// serviceMatrix builds one row per service with per-region status and skew.
func (a *Aggregator) serviceMatrix(services []models.Service, regions []models.Region) []models.ServiceHealth {
	out := make([]models.ServiceHealth, 0, len(services))

	for _, svc := range services {
		row := models.ServiceHealth{
			ServiceID: svc.ID,
			Name:      svc.Name,
			Runtime:   svc.Runtime,
			ByRegion:  map[string]models.Status{},
			Versions:  map[string]string{},
		}

		versions := map[string]struct{}{}
		for _, sr := range svc.Regions {
			st, ok := a.prober.Status(svc.ID, sr.RegionID)
			if !ok {
				st = models.StatusUnknown
			}
			row.ByRegion[sr.RegionID] = st
			row.Versions[sr.RegionID] = sr.Version
			if sr.Version != "" {
				versions[sr.Version] = struct{}{}
			}
		}
		row.Skew = len(versions) > 1
		row.Note = skewNote(row, svc)
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func skewNote(row models.ServiceHealth, svc models.Service) string {
	if !row.Skew {
		if svc.Role == models.RoleHealthcheckOnly {
			return "Monitor only — does not influence routing"
		}
		return ""
	}
	pairs := make([]string, 0, len(row.Versions))
	for region, v := range row.Versions {
		if v != "" {
			pairs = append(pairs, fmt.Sprintf("%s on %s", region, v))
		}
	}
	sort.Strings(pairs)
	return "Version skew: " + joinAnd(pairs)
}

func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1 : len(parts)-1] {
		out += ", " + p
	}
	return out + " and " + parts[len(parts)-1]
}

// regionMetrics derives p99-ish latency and an error rate from recent probes.
func regionMetrics(regionID string, services []models.Service, latest map[string]models.ProbeResult) (int, float64, time.Time) {
	var (
		latencies []int
		failed    int
		total     int
		newest    time.Time
	)
	for _, svc := range services {
		p, ok := latest[svc.ID+"|"+regionID]
		if !ok {
			continue
		}
		total++
		if p.LatencyMs > 0 {
			latencies = append(latencies, p.LatencyMs)
		}
		if p.Status == models.StatusCritical || p.Status == models.StatusDegraded {
			failed++
		}
		if p.ProbedAt.After(newest) {
			newest = p.ProbedAt
		}
	}
	if total == 0 {
		return 0, 0, newest
	}

	sort.Ints(latencies)
	p99 := 0
	if n := len(latencies); n > 0 {
		idx := int(float64(n) * 0.99)
		if idx >= n {
			idx = n - 1
		}
		p99 = latencies[idx]
	}
	return p99, round2(100 * float64(failed) / float64(total)), newest
}

// replicationLag reads the lag reported by the region's datastore probe, if a
// service named like a database exposes one in its probe detail.
func replicationLag(regionID string, latest map[string]models.ProbeResult) int {
	for key, p := range latest {
		if len(key) < len(regionID)+1 || key[len(key)-len(regionID):] != regionID {
			continue
		}
		if v, ok := parseLagMs(p.Detail); ok {
			return v
		}
	}
	return 0
}

// parseLagMs extracts "lag_ms=<n>" from a probe detail line.
func parseLagMs(detail string) (int, bool) {
	const key = "lag_ms="
	i := indexOf(detail, key)
	if i < 0 {
		return 0, false
	}
	rest := detail[i+len(key):]
	n := 0
	digits := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	return n, true
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// dominantVersion returns the most common deployed version in a region.
func dominantVersion(regionID string, services []models.Service) string {
	counts := map[string]int{}
	for _, svc := range services {
		for _, sr := range svc.Regions {
			if sr.RegionID == regionID && sr.Version != "" {
				counts[sr.Version]++
			}
		}
	}
	best, bestN := "", 0
	for v, n := range counts {
		if n > bestN || (n == bestN && v > best) {
			best, bestN = v, n
		}
	}
	return best
}

// azSummary renders "n / m" from the region's AZ-scoped dependencies.
func azSummary(deps []models.DependencyStatus) string {
	total, healthy := 0, 0
	for _, d := range deps {
		total++
		if d.Status == models.StatusHealthy {
			healthy++
		}
	}
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%d / %d", healthy, total)
}

func servesRegion(svc models.Service, regionID string) bool {
	for _, sr := range svc.Regions {
		if sr.RegionID == regionID {
			return true
		}
	}
	return false
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func orFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}
