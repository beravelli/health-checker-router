//go:build integration

// Store is tested against a real Postgres. Mocking a database proves that the
// code calls the mock, which is not the risk — the risk is the SQL, the
// partitioning, the constraints and the upsert semantics. Run with:
//
//	go test -tags=integration ./internal/store/
//
// TEST_DATABASE_URL must point at a disposable database with the migrations in
// db/migrations already applied. CI does this with a Postgres service container
// and a goose step; see .github/workflows/ci.yml.
package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/beravelli/health-checker-router/internal/models"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// cleanup removes rows this test created, so tests can run repeatedly against
// the same database without interfering.
func cleanup(t *testing.T, s *Store, serviceIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range serviceIDs {
			_ = s.DeleteService(ctx, id)
		}
	})
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestPingAndSeededRegions(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	if err := s.Ping(c); err != nil {
		t.Fatalf("ping: %v", err)
	}

	regions, err := s.ListRegions(c)
	if err != nil {
		t.Fatalf("list regions: %v", err)
	}
	// The seed migration creates both regions; the whole design assumes two.
	if len(regions) < 2 {
		t.Errorf("got %d regions, want the 2 seeded ones — is 00002_seed.sql applied?", len(regions))
	}
}

// TestNewServiceDefaultsToMonitorOnly is design principle 7 enforced at the
// storage layer: a newly registered service must not be able to move traffic,
// whatever the caller asked for.
func TestNewServiceDefaultsToMonitorOnly(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	created, err := s.CreateService(c, models.Service{
		Name:      "integration-default-role",
		ProbeType: models.ProbeHTTP,
		Regions:   []models.ServiceRegion{{RegionID: "us-east-1", Endpoint: "http://localhost"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cleanup(t, s, created.ID)

	if created.Role != models.RoleHealthcheckOnly {
		t.Errorf("role = %q, want %q for a newly created service",
			created.Role, models.RoleHealthcheckOnly)
	}
}

func TestServiceRoundTripAndRoleChange(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	created, err := s.CreateService(c, models.Service{
		Name:      "integration-roundtrip",
		ProbeType: models.ProbeHTTP,
		Regions: []models.ServiceRegion{
			{RegionID: "us-east-1", Endpoint: "http://east", Version: "v1"},
			{RegionID: "us-west-2", Endpoint: "http://west", Version: "v1"},
		},
		ProbeConfig: map[string]any{"path": "/healthz", "expectedStatus": float64(200)},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cleanup(t, s, created.ID)

	got, err := s.GetService(c, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "integration-roundtrip" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.Regions) != 2 {
		t.Fatalf("got %d regions, want 2", len(got.Regions))
	}
	// probe_config is JSONB; confirm it survives the round trip.
	if got.ProbeConfig["path"] != "/healthz" {
		t.Errorf("probeConfig[path] = %v, want /healthz", got.ProbeConfig["path"])
	}

	// Promotion to traffic-moving is a separate, explicit step.
	if err := s.UpdateServiceRole(c, created.ID, models.RoleHealthcheckFailover); err != nil {
		t.Fatalf("update role: %v", err)
	}
	got, err = s.GetService(c, created.ID)
	if err != nil {
		t.Fatalf("get after role change: %v", err)
	}
	if got.Role != models.RoleHealthcheckFailover {
		t.Errorf("role = %q after promotion", got.Role)
	}
}

func TestGetServiceNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetService(ctx(t), "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Error("expected an error for a missing service")
	}
}

// TestProbeResultsRequirePartition exercises the partitioned probe_results
// table. EnsurePartitions must run before a write lands, or the insert fails
// with "no partition of relation found" — the failure mode the hourly
// maintenance loop exists to prevent.
func TestProbeResultsRequirePartition(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	if err := s.EnsurePartitions(c, 7); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}

	created, err := s.CreateService(c, models.Service{
		Name:      "integration-probes",
		ProbeType: models.ProbeHTTP,
		Regions:   []models.ServiceRegion{{RegionID: "us-east-1", Endpoint: "http://localhost"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cleanup(t, s, created.ID)

	for _, st := range []models.Status{models.StatusHealthy, models.StatusDegraded, models.StatusUnknown} {
		if err := s.RecordProbe(c, models.ProbeResult{
			ServiceID: created.ID,
			RegionID:  "us-east-1",
			Status:    st,
			LatencyMs: 12,
			Detail:    "integration test",
			ProbedAt:  time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record %s: %v", st, err)
		}
	}

	latest, err := s.LatestProbeByServiceRegion(c)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	got, ok := latest[created.ID+"|us-east-1"]
	if !ok {
		t.Fatal("no latest probe recorded for the target")
	}
	// The most recent write wins, and unknown is persisted as itself rather
	// than being normalised away.
	if got.Status != models.StatusUnknown {
		t.Errorf("latest status = %s, want unknown", got.Status)
	}
}

func TestEnsurePartitionsIsIdempotent(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	// Runs hourly in production, so re-running must be a no-op rather than an
	// error about an existing partition.
	for i := 0; i < 3; i++ {
		if err := s.EnsurePartitions(c, 7); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
}

func TestRefreshRollup(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	if err := s.EnsurePartitions(c, 7); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if err := s.RefreshRollup(c); err != nil {
		t.Fatalf("refresh rollup: %v", err)
	}
}

// ------------------------------------------------------------- lb weights

func TestWeightsRoundTrip(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	if err := s.SetWeights(c, map[string]int{"us-east-1": 70, "us-west-2": 30}, true); err != nil {
		t.Fatalf("set weights: %v", err)
	}
	weights, err := s.GetWeights(c)
	if err != nil {
		t.Fatalf("get weights: %v", err)
	}

	byRegion := map[string]models.LBWeight{}
	for _, w := range weights {
		byRegion[w.RegionID] = w
	}
	if got := byRegion["us-east-1"]; got.Weight != 70 || !got.IsAutomatic {
		t.Errorf("us-east-1 = %+v, want weight 70 automatic", got)
	}

	// A manual override must be recorded as manual, so the console can show
	// that automation is not in control.
	if err := s.SetWeights(c, map[string]int{"us-east-1": 100, "us-west-2": 0}, false); err != nil {
		t.Fatalf("set manual: %v", err)
	}
	weights, err = s.GetWeights(c)
	if err != nil {
		t.Fatalf("get weights: %v", err)
	}
	for _, w := range weights {
		if w.IsAutomatic {
			t.Errorf("%s still marked automatic after a manual override", w.RegionID)
		}
	}

	t.Cleanup(func() {
		_ = s.SetWeights(context.Background(), map[string]int{"us-east-1": 50, "us-west-2": 50}, true)
	})
}

// --------------------------------------------------------------- fail-open

func TestFailOpenLifecycle(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	// Start from a clean slate; a leftover window from another test would make
	// this ambiguous.
	if err := s.ResolveFailOpen(c); err != nil {
		t.Fatalf("pre-resolve: %v", err)
	}
	if active, err := s.ActiveFailOpen(c); err != nil {
		t.Fatalf("active: %v", err)
	} else if active != nil {
		t.Fatal("expected no active window after resolving")
	}

	opened, err := s.OpenFailOpen(c, "integration test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !opened.Active() {
		t.Error("a newly opened window should be active")
	}

	active, err := s.ActiveFailOpen(c)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if active == nil || active.Reason != "integration test" {
		t.Fatalf("active = %+v", active)
	}

	if err := s.ResolveFailOpen(c); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if active, err := s.ActiveFailOpen(c); err != nil || active != nil {
		t.Errorf("active = %+v err = %v; want nil after resolve", active, err)
	}
}

// ---------------------------------------------------------------- audit

func TestAuditIsAppendOnlyAndOrdered(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	for _, action := range []string{"integration.first", "integration.second"} {
		if err := s.Audit(c, "tester", action, "target", map[string]any{"k": "v"}); err != nil {
			t.Fatalf("audit %s: %v", action, err)
		}
	}

	entries, err := s.ListAudit(c, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("got %d entries, want at least 2", len(entries))
	}
	// Newest first, so the console shows recent activity without paging.
	if !entries[0].At.After(entries[1].At) && !entries[0].At.Equal(entries[1].At) {
		t.Error("audit entries are not ordered newest first")
	}
}

func TestRecentIncidentExistsDeduplicates(t *testing.T) {
	s := testStore(t)
	c := ctx(t)

	title := "integration dedupe probe " + time.Now().Format(time.RFC3339Nano)
	if exists, err := s.RecentIncidentExists(c, title, time.Hour); err != nil {
		t.Fatalf("exists: %v", err)
	} else if exists {
		t.Fatal("a never-created incident should not exist")
	}

	if _, err := s.CreateIncident(c, models.Incident{
		Severity: models.SeverityWarning,
		Title:    title,
		RegionID: "us-east-1",
	}); err != nil {
		t.Fatalf("create incident: %v", err)
	}

	// Dedupe is what stops a flapping probe from paging on-call repeatedly.
	if exists, err := s.RecentIncidentExists(c, title, time.Hour); err != nil {
		t.Fatalf("exists after create: %v", err)
	} else if !exists {
		t.Error("a just-created incident should be found within the window")
	}
	if exists, err := s.RecentIncidentExists(c, title, time.Nanosecond); err != nil {
		t.Fatalf("exists narrow window: %v", err)
	} else if exists {
		t.Error("the incident should fall outside a 1ns window")
	}
}
