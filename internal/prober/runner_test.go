package prober

import (
	"testing"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
)

// newRatioRunner builds a Runner with hand-placed states, bypassing Sync so the
// ratio arithmetic can be tested without starting probe goroutines.
func newRatioRunner(threshold int, states map[string]*State) *Runner {
	return &Runner{
		cfg:    &config.Config{UnknownsToDistrust: threshold},
		states: states,
	}
}

// observe drives a state through a sequence of outcomes.
func observe(s *State, outcomes ...models.Status) *State {
	for _, o := range outcomes {
		s.Observe(o)
	}
	return s
}

func healthy() *State {
	return observe(NewState(3, 2), models.StatusHealthy, models.StatusHealthy)
}

func TestUnknownRatioIgnoresOtherRegions(t *testing.T) {
	r := newRatioRunner(3, map[string]*State{
		"svc-a|us-east-1": healthy(),
		"svc-b|us-east-1": healthy(),
		"svc-a|us-west-2": NewState(3, 2), // never probed, but a different region
	})

	if got := r.UnknownRatio("us-east-1"); got != 0 {
		t.Errorf("us-east-1 ratio = %.2f, want 0", got)
	}
	if got := r.UnknownRatio("us-west-2"); got != 1 {
		t.Errorf("us-west-2 ratio = %.2f, want 1", got)
	}
}

func TestUnknownRatioEmptyRegionReportsFullVisibility(t *testing.T) {
	r := newRatioRunner(3, map[string]*State{})
	if got := r.UnknownRatio("us-east-1"); got != 0 {
		t.Errorf("empty region ratio = %.2f, want 0 — nothing to be blind about", got)
	}
}

// TestUnknownRatioNeedsSustainedUnknowns is the debounce, observed through the
// interface the fail-open controller actually calls.
func TestUnknownRatioNeedsSustainedUnknowns(t *testing.T) {
	blipped := healthy()
	r := newRatioRunner(3, map[string]*State{
		"svc-a|us-east-1": blipped,
		"svc-b|us-east-1": healthy(),
		"svc-c|us-east-1": healthy(),
	})

	blipped.Observe(models.StatusUnknown)
	if got := r.UnknownRatio("us-east-1"); got != 0 {
		t.Errorf("one blip moved the ratio to %.2f; unknowns are debounced", got)
	}

	blipped.Observe(models.StatusUnknown)
	blipped.Observe(models.StatusUnknown)
	if got, want := r.UnknownRatio("us-east-1"), 1.0/3.0; got != want {
		t.Errorf("ratio = %.4f, want %.4f after a sustained unknown streak", got, want)
	}
}

// TestUnknownRatioOnColdStart is the restart-mid-incident case: before any
// probe lands, the region reports fully inconclusive, which distrusts it and
// holds weights rather than reconciling against invented health.
func TestUnknownRatioOnColdStart(t *testing.T) {
	r := newRatioRunner(3, map[string]*State{
		"svc-a|us-east-1": NewState(3, 2),
		"svc-b|us-east-1": NewState(3, 2),
	})
	if got := r.UnknownRatio("us-east-1"); got != 1 {
		t.Fatalf("cold-start ratio = %.2f, want 1", got)
	}

	// As probes land, visibility returns.
	r.states["svc-a|us-east-1"] = healthy()
	if got := r.UnknownRatio("us-east-1"); got != 0.5 {
		t.Errorf("ratio = %.2f, want 0.5 with one of two targets established", got)
	}
}
