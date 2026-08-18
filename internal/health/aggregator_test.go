package health

import (
	"math"
	"testing"

	"github.com/beravelli/health-checker-router/internal/models"
)

// fakeProber returns canned per-target statuses. Keyed "serviceID|regionID";
// a missing key means the target has never been observed.
type fakeProber map[string]models.Status

func (f fakeProber) Status(serviceID, regionID string) (models.Status, bool) {
	st, ok := f[serviceID+"|"+regionID]
	return st, ok
}

// testAggregator builds one with no store: regionScore, classify and
// serviceMatrix do not touch it.
func testAggregator(p StatusProvider) *Aggregator {
	return &Aggregator{prober: p, weights: DefaultWeights(), thresholds: DefaultThresholds()}
}

func svc(id string, role models.ProbeRole, regions ...string) models.Service {
	s := models.Service{ID: id, Name: id, Role: role}
	for _, r := range regions {
		s.Regions = append(s.Regions, models.ServiceRegion{RegionID: r})
	}
	return s
}

func dep(region, name string, st models.Status) models.DependencyStatus {
	return models.DependencyStatus{RegionID: region, Name: name, Status: st}
}

const epsilon = 1e-9

func closeTo(got, want float64) bool { return math.Abs(got-want) < epsilon }

// TestMonitorOnlyServicesCannotMoveTraffic is the safety property behind design
// principle 7: registering a service for observation must never affect routing.
func TestMonitorOnlyServicesCannotMoveTraffic(t *testing.T) {
	a := testAggregator(fakeProber{
		"broken|us-east-1": models.StatusCritical,
	})

	// A totally dead monitor-only service, and nothing else.
	score := a.regionScore("us-east-1",
		[]models.Service{svc("broken", models.RoleHealthcheckOnly, "us-east-1")},
		nil)

	if !closeTo(score, 1.0) {
		t.Errorf("score = %.4f; a monitor-only service must not reduce it", score)
	}
	if got := a.classify(score); got != models.StatusHealthy {
		t.Errorf("status = %s, want healthy", got)
	}
}

func TestFailoverEligibleServiceDragsScore(t *testing.T) {
	a := testAggregator(fakeProber{
		"api|us-east-1": models.StatusCritical,
	})
	score := a.regionScore("us-east-1",
		[]models.Service{svc("api", models.RoleHealthcheckFailover, "us-east-1")},
		nil)

	// No dependency observations, so services carry the score alone.
	if !closeTo(score, 0.0) {
		t.Errorf("score = %.4f, want 0", score)
	}
}

// TestWeightingIs60_40 pins the documented split. Design principle 1 says
// services 60%, dependencies 40%, and the LB weight follows this number.
func TestWeightingIs60_40(t *testing.T) {
	a := testAggregator(fakeProber{
		"api|us-east-1": models.StatusCritical, // 0.0
	})
	score := a.regionScore("us-east-1",
		[]models.Service{svc("api", models.RoleHealthcheckFailover, "us-east-1")},
		[]models.DependencyStatus{dep("us-east-1", "postgres", models.StatusHealthy)}, // 1.0
	)

	// 0.6*0 + 0.4*1
	if !closeTo(score, 0.4) {
		t.Errorf("score = %.4f, want 0.40 (0.6*0 + 0.4*1)", score)
	}
	if got := a.classify(score); got != models.StatusCritical {
		t.Errorf("status = %s, want critical", got)
	}
}

func TestDegradedDependencyReducesButDoesNotEvict(t *testing.T) {
	// Design principle 1: a region with a degraded cache loses some traffic,
	// it is not evicted.
	a := testAggregator(fakeProber{"api|us-east-1": models.StatusHealthy})
	score := a.regionScore("us-east-1",
		[]models.Service{svc("api", models.RoleHealthcheckFailover, "us-east-1")},
		[]models.DependencyStatus{dep("us-east-1", "redis", models.StatusDegraded)}, // 0.5
	)

	// 0.6*1 + 0.4*0.5 = 0.80 → degraded, not critical.
	if !closeTo(score, 0.8) {
		t.Errorf("score = %.4f, want 0.80", score)
	}
	if got := a.classify(score); got != models.StatusDegraded {
		t.Errorf("status = %s, want degraded — a degraded cache must not evict the region", got)
	}
}

// TestUnknownIsExcludedNotCounted covers design principle 3 at the scoring
// layer: an inconclusive target is left out of the average entirely rather than
// being treated as healthy or as failed. Fail-open handles it separately.
func TestUnknownIsExcludedNotCounted(t *testing.T) {
	services := []models.Service{
		svc("good", models.RoleHealthcheckFailover, "us-east-1"),
		svc("blind", models.RoleHealthcheckFailover, "us-east-1"),
	}

	withUnknown := testAggregator(fakeProber{
		"good|us-east-1":  models.StatusHealthy,
		"blind|us-east-1": models.StatusUnknown,
	}).regionScore("us-east-1", services, nil)

	// Same inputs with the unknown target simply absent.
	withAbsent := testAggregator(fakeProber{
		"good|us-east-1": models.StatusHealthy,
	}).regionScore("us-east-1", services, nil)

	if !closeTo(withUnknown, withAbsent) {
		t.Errorf("unknown scored %.4f but absent scored %.4f; they must be equivalent",
			withUnknown, withAbsent)
	}
	if !closeTo(withUnknown, 1.0) {
		t.Errorf("score = %.4f, want 1.0 — the one conclusive target is healthy", withUnknown)
	}
}

// TestNoObservationsScoresPerfect documents a deliberate and slightly
// uncomfortable choice: with nothing observed at all the score is 1.0.
//
// It is safe only because the fail-open controller runs alongside this and will
// distrust a region whose targets are all inconclusive. The score alone is not
// a safety mechanism — if that pairing is ever broken, this becomes a way to
// send traffic at a region nobody has probed.
func TestNoObservationsScoresPerfect(t *testing.T) {
	a := testAggregator(fakeProber{})
	if score := a.regionScore("us-east-1", nil, nil); !closeTo(score, 1.0) {
		t.Errorf("score = %.4f, want 1.0", score)
	}
}

// TestOneSidedObservationsAreNotDiluted: when only dependencies have been
// observed, they carry the score rather than being blended against an
// assumed-perfect service half.
func TestOneSidedObservationsAreNotDiluted(t *testing.T) {
	a := testAggregator(fakeProber{})

	depsOnly := a.regionScore("us-east-1", nil,
		[]models.DependencyStatus{dep("us-east-1", "postgres", models.StatusCritical)})
	if !closeTo(depsOnly, 0.0) {
		t.Errorf("deps-only score = %.4f, want 0 — not 0.4 from dilution", depsOnly)
	}

	svcOnly := testAggregator(fakeProber{"api|us-east-1": models.StatusCritical}).
		regionScore("us-east-1",
			[]models.Service{svc("api", models.RoleHealthcheckFailover, "us-east-1")}, nil)
	if !closeTo(svcOnly, 0.0) {
		t.Errorf("services-only score = %.4f, want 0 — not 0.6 from dilution", svcOnly)
	}
}

// TestServicesNotInRegionAreIgnored: a service registered only in us-west-2
// must not affect the us-east-1 score.
func TestServicesNotInRegionAreIgnored(t *testing.T) {
	a := testAggregator(fakeProber{"api|us-west-2": models.StatusCritical})
	score := a.regionScore("us-east-1",
		[]models.Service{svc("api", models.RoleHealthcheckFailover, "us-west-2")}, nil)

	if !closeTo(score, 1.0) {
		t.Errorf("score = %.4f; another region's service must not count", score)
	}
}

func TestClassifyBoundaries(t *testing.T) {
	a := testAggregator(fakeProber{})
	cases := []struct {
		score float64
		want  models.Status
	}{
		{1.00, models.StatusHealthy},
		{0.95, models.StatusHealthy}, // inclusive lower bound
		{0.9499, models.StatusDegraded},
		{0.70, models.StatusDegraded}, // inclusive lower bound
		{0.6999, models.StatusCritical},
		{0.00, models.StatusCritical},
	}
	for _, tc := range cases {
		if got := a.classify(tc.score); got != tc.want {
			t.Errorf("classify(%.4f) = %s, want %s", tc.score, got, tc.want)
		}
	}
}

func TestMultipleServicesAverage(t *testing.T) {
	a := testAggregator(fakeProber{
		"a|us-east-1": models.StatusHealthy,  // 1.0
		"b|us-east-1": models.StatusDegraded, // 0.5
		"c|us-east-1": models.StatusCritical, // 0.0
	})
	score := a.regionScore("us-east-1", []models.Service{
		svc("a", models.RoleHealthcheckFailover, "us-east-1"),
		svc("b", models.RoleHealthcheckFailover, "us-east-1"),
		svc("c", models.RoleHealthcheckFailover, "us-east-1"),
	}, nil)

	if !closeTo(score, 0.5) {
		t.Errorf("score = %.4f, want 0.5 (mean of 1.0, 0.5, 0.0)", score)
	}
}

// ---------------------------------------------------------- service matrix

func TestServiceMatrixReportsUnobservedAsUnknown(t *testing.T) {
	a := testAggregator(fakeProber{"api|us-east-1": models.StatusHealthy})
	rows := a.serviceMatrix(
		[]models.Service{svc("api", models.RoleHealthcheckFailover, "us-east-1", "us-west-2")},
		nil)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := rows[0].ByRegion["us-east-1"]; got != models.StatusHealthy {
		t.Errorf("us-east-1 = %s, want healthy", got)
	}
	if got := rows[0].ByRegion["us-west-2"]; got != models.StatusUnknown {
		t.Errorf("us-west-2 = %s, want unknown for an unobserved target", got)
	}
}

func TestServiceMatrixFlagsVersionSkew(t *testing.T) {
	s := models.Service{
		ID: "api", Name: "api", Role: models.RoleHealthcheckFailover,
		Regions: []models.ServiceRegion{
			{RegionID: "us-east-1", Version: "v2.1.0"},
			{RegionID: "us-west-2", Version: "v2.0.9"},
		},
	}
	rows := testAggregator(fakeProber{}).serviceMatrix([]models.Service{s}, nil)

	if !rows[0].Skew {
		t.Error("differing versions across regions should flag skew")
	}
	if rows[0].Note == "" {
		t.Error("skew should carry an explanatory note")
	}
}

func TestServiceMatrixNoSkewWhenVersionsMatch(t *testing.T) {
	s := models.Service{
		ID: "api", Name: "api", Role: models.RoleHealthcheckFailover,
		Regions: []models.ServiceRegion{
			{RegionID: "us-east-1", Version: "v2.1.0"},
			{RegionID: "us-west-2", Version: "v2.1.0"},
		},
	}
	rows := testAggregator(fakeProber{}).serviceMatrix([]models.Service{s}, nil)
	if rows[0].Skew {
		t.Error("matching versions should not flag skew")
	}
}

func TestServiceMatrixNotesMonitorOnly(t *testing.T) {
	rows := testAggregator(fakeProber{}).serviceMatrix(
		[]models.Service{svc("watcher", models.RoleHealthcheckOnly, "us-east-1")}, nil)

	if rows[0].Note == "" {
		t.Error("a monitor-only service should say so, since it looks identical otherwise")
	}
}

func TestServiceMatrixSortsByName(t *testing.T) {
	rows := testAggregator(fakeProber{}).serviceMatrix([]models.Service{
		svc("zebra", models.RoleHealthcheckFailover, "us-east-1"),
		svc("alpha", models.RoleHealthcheckFailover, "us-east-1"),
		svc("middle", models.RoleHealthcheckFailover, "us-east-1"),
	}, nil)

	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if rows[i].Name != w {
			t.Errorf("row %d = %s, want %s", i, rows[i].Name, w)
		}
	}
}
