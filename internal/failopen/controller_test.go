package failopen

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/models"
)

// fakeStore is an in-memory Store holding at most one open window.
type fakeStore struct {
	active   *models.FailOpenEvent
	opens    int
	resolves int
}

func (f *fakeStore) ActiveFailOpen(context.Context) (*models.FailOpenEvent, error) {
	return f.active, nil
}

func (f *fakeStore) OpenFailOpen(_ context.Context, reason string) (*models.FailOpenEvent, error) {
	f.opens++
	f.active = &models.FailOpenEvent{ID: int64(f.opens), TriggeredAt: time.Now(), Reason: reason}
	return f.active, nil
}

func (f *fakeStore) ResolveFailOpen(context.Context) error {
	f.resolves++
	f.active = nil
	return nil
}

// fakePager records what on-call would have been told.
type fakePager struct {
	pages []models.Severity
}

func (p *fakePager) Page(_ context.Context, sev models.Severity, _, _, _ string) error {
	p.pages = append(p.pages, sev)
	return nil
}

// ratios is a canned UnknownRatioProvider.
type ratios map[string]float64

func (r ratios) UnknownRatio(regionID string) float64 { return r[regionID] }

func newTestController(t *testing.T, p Policy) (*Controller, *fakeStore, *fakePager) {
	t.Helper()
	store := &fakeStore{}
	pager := &fakePager{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(p, store, pager, events.NewHub(), log), store, pager
}

var twoRegions = []models.Region{{ID: "us-east-1"}, {ID: "us-west-2"}}

// TestUnknownToleranceMatchesQuorumRule pins the arithmetic that decides
// whether a region's signal is usable.
//
// This is a regression test. The tolerance previously carried a +1 in the
// numerator and returned 2/3 for the 2-of-3 default, so a region kept driving
// traffic weights until two of every three probes had gone dark.
func TestUnknownToleranceMatchesQuorumRule(t *testing.T) {
	cases := []struct {
		required, total int
		want            float64
	}{
		{2, 3, 1.0 / 3.0}, // the default: lose one of three and quorum still holds
		{1, 2, 1.0 / 2.0},
		{3, 4, 1.0 / 4.0},
		{2, 5, 3.0 / 5.0},
	}
	for _, tc := range cases {
		c, _, _ := newTestController(t, Policy{QuorumRequired: tc.required, QuorumTotal: tc.total})
		if got := c.unknownTolerance(); got != tc.want {
			t.Errorf("%d-of-%d tolerance = %.4f, want %.4f", tc.required, tc.total, got, tc.want)
		}
	}
}

func TestRegionTrustedAtExactlyTolerance(t *testing.T) {
	// One inconclusive target in three is exactly 2-of-3 quorum: still trusted.
	c, store, _ := newTestController(t, Policy{QuorumRequired: 2, QuorumTotal: 3})

	d, err := c.Evaluate(context.Background(), twoRegions,
		ratios{"us-east-1": 1.0 / 3.0, "us-west-2": 0})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !d.Trusted["us-east-1"] {
		t.Error("region at exactly the tolerance should stay trusted")
	}
	if d.FailOpen {
		t.Error("fail-open opened while quorum was still satisfied")
	}
	if store.opens != 0 {
		t.Errorf("opened %d fail-open windows, want 0", store.opens)
	}
}

func TestRegionDistrustedJustPastTolerance(t *testing.T) {
	// Two of three inconclusive breaks 2-of-3 quorum. Under the old +1
	// tolerance this case was wrongly trusted.
	c, store, pager := newTestController(t, Policy{QuorumRequired: 2, QuorumTotal: 3})

	d, err := c.Evaluate(context.Background(), twoRegions,
		ratios{"us-east-1": 2.0 / 3.0, "us-west-2": 0})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Trusted["us-east-1"] {
		t.Error("region past the tolerance should not be trusted")
	}
	if !d.Trusted["us-west-2"] {
		t.Error("the healthy region should be unaffected")
	}
	if !d.FailOpen {
		t.Fatal("fail-open should be active")
	}
	if store.opens != 1 {
		t.Errorf("opened %d windows, want 1", store.opens)
	}
	if len(pager.pages) != 1 || pager.pages[0] != models.SeverityWarning {
		t.Errorf("pages = %v, want one warning", pager.pages)
	}
}

// TestCheckerSideOutageDoesNotFailOver is the scenario the package exists for:
// the checker loses the network, every probe everywhere goes inconclusive, and
// the correct response is to hold weights and page — never to evict a region.
func TestCheckerSideOutageDoesNotFailOver(t *testing.T) {
	c, _, pager := newTestController(t, Policy{QuorumRequired: 2, QuorumTotal: 3, Hold: time.Hour})

	d, err := c.Evaluate(context.Background(), twoRegions,
		ratios{"us-east-1": 1.0, "us-west-2": 1.0})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	for _, r := range twoRegions {
		if d.Trusted[r.ID] {
			t.Errorf("%s trusted while totally blind", r.ID)
		}
	}
	if !d.FailOpen || d.HoldExpired {
		t.Errorf("want fail-open held and not expired, got failOpen=%v holdExpired=%v",
			d.FailOpen, d.HoldExpired)
	}
	if len(pager.pages) != 1 || pager.pages[0] != models.SeverityWarning {
		t.Errorf("pages = %v, want one warning", pager.pages)
	}
}

func TestHoldExpiryEscalatesOnceThenStopsPaging(t *testing.T) {
	c, store, pager := newTestController(t, Policy{QuorumRequired: 2, QuorumTotal: 3, Hold: time.Minute})
	blind := ratios{"us-east-1": 1.0, "us-west-2": 0}

	if _, err := c.Evaluate(context.Background(), twoRegions, blind); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// Backdate the window past the hold.
	store.active.TriggeredAt = time.Now().Add(-2 * time.Minute)

	d, err := c.Evaluate(context.Background(), twoRegions, blind)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !d.HoldExpired {
		t.Fatal("hold should have expired")
	}
	if len(pager.pages) != 2 || pager.pages[1] != models.SeverityCritical {
		t.Fatalf("pages = %v, want warning then critical", pager.pages)
	}

	// Further ticks in the same window must not re-page.
	for i := 0; i < 5; i++ {
		if _, err := c.Evaluate(context.Background(), twoRegions, blind); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
	}
	if len(pager.pages) != 2 {
		t.Errorf("hold expiry paged %d times, want once per window", len(pager.pages)-1)
	}
}

func TestRecoveryClosesWindowAndResolves(t *testing.T) {
	c, store, pager := newTestController(t, Policy{QuorumRequired: 2, QuorumTotal: 3, Hold: time.Hour})

	if _, err := c.Evaluate(context.Background(), twoRegions, ratios{"us-east-1": 1.0}); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	d, err := c.Evaluate(context.Background(), twoRegions, ratios{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.FailOpen {
		t.Error("fail-open should have closed once probes were conclusive again")
	}
	if store.resolves != 1 {
		t.Errorf("resolved %d times, want 1", store.resolves)
	}
	if len(pager.pages) != 2 || pager.pages[1] != models.SeverityResolved {
		t.Errorf("pages = %v, want warning then resolved", pager.pages)
	}
}

// TestReopenAfterRecoveryPagesAgain guards the pagedForHold reset: a second
// window is a new incident and must escalate on its own merits.
func TestReopenAfterRecoveryPagesAgain(t *testing.T) {
	c, store, _ := newTestController(t, Policy{QuorumRequired: 2, QuorumTotal: 3, Hold: time.Minute})
	blind := ratios{"us-east-1": 1.0}

	if _, err := c.Evaluate(context.Background(), twoRegions, blind); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	store.active.TriggeredAt = time.Now().Add(-2 * time.Minute)
	if _, err := c.Evaluate(context.Background(), twoRegions, blind); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if _, err := c.Evaluate(context.Background(), twoRegions, ratios{}); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// New window, immediately past its hold.
	if _, err := c.Evaluate(context.Background(), twoRegions, blind); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	store.active.TriggeredAt = time.Now().Add(-2 * time.Minute)
	d, err := c.Evaluate(context.Background(), twoRegions, blind)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !d.HoldExpired {
		t.Error("the second window should be able to expire its own hold")
	}
	if store.opens != 2 {
		t.Errorf("opened %d windows, want 2", store.opens)
	}
}

func TestPolicyDefaultsAreSane(t *testing.T) {
	c, _, _ := newTestController(t, Policy{})
	p := c.Policy()
	if p.QuorumRequired != 2 || p.QuorumTotal != 3 || p.Hold != 10*time.Minute {
		t.Fatalf("defaults = %+v, want 2-of-3 with a 10m hold", p)
	}
	if got, want := c.unknownTolerance(), 1.0/3.0; got != want {
		t.Errorf("default tolerance = %.4f, want %.4f", got, want)
	}
}
