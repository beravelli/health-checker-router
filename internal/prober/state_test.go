package prober

import (
	"testing"

	"github.com/beravelli/health-checker-router/internal/models"
)

// healthyState returns a state machine that has been probed into the healthy
// steady state, which is where most of these tests want to start. A fresh
// NewState is seeded unknown and must earn health first.
func healthyState(t *testing.T, failuresToTrip, successToRecover int) *State {
	t.Helper()
	s := NewState(failuresToTrip, successToRecover)
	for i := 0; i < successToRecover; i++ {
		s.Observe(models.StatusHealthy)
	}
	if got := s.Current(); got != models.StatusHealthy {
		t.Fatalf("seeding healthy failed: state is %s", got)
	}
	return s
}

func TestFlappingDoesNotTrip(t *testing.T) {
	// The flapping edge case: alternating pass/fail near a threshold must never
	// reach the tripped state, because the failure counter resets on each pass.
	s := healthyState(t, 3, 2)
	for i := 0; i < 20; i++ {
		s.Observe(models.StatusCritical)
		s.Observe(models.StatusHealthy)
	}
	if got := s.Current(); got == models.StatusCritical {
		t.Fatalf("flapping tripped the target to critical; want never critical")
	}
}

func TestTripsAfterThreeConsecutiveFailures(t *testing.T) {
	s := healthyState(t, 3, 2)

	if got, _ := s.Observe(models.StatusCritical); got != models.StatusDegraded {
		t.Fatalf("after 1 failure got %s, want degraded", got)
	}
	if got, _ := s.Observe(models.StatusCritical); got != models.StatusDegraded {
		t.Fatalf("after 2 failures got %s, want degraded", got)
	}
	got, changed := s.Observe(models.StatusCritical)
	if got != models.StatusCritical {
		t.Fatalf("after 3 failures got %s, want critical", got)
	}
	if !changed {
		t.Fatal("third failure should report a status change")
	}
}

func TestRecoversOnlyAfterTwoSuccesses(t *testing.T) {
	s := healthyState(t, 3, 2)
	for i := 0; i < 3; i++ {
		s.Observe(models.StatusCritical)
	}

	if got, _ := s.Observe(models.StatusHealthy); got != models.StatusCritical {
		t.Fatalf("one success recovered too early: got %s, want critical", got)
	}
	if got, _ := s.Observe(models.StatusHealthy); got != models.StatusHealthy {
		t.Fatalf("two successes should recover: got %s, want healthy", got)
	}
}

func TestUnknownDoesNotAffectCounters(t *testing.T) {
	// Unknown is "we could not tell", not "it failed". It must not advance the
	// trip counter, and it must not clear an in-flight failure streak either.
	s := healthyState(t, 3, 2)
	s.Observe(models.StatusCritical)
	s.Observe(models.StatusCritical)

	for i := 0; i < 5; i++ {
		if got, _ := s.Observe(models.StatusUnknown); got == models.StatusCritical {
			t.Fatal("unknown probes tripped the target; they must not")
		}
	}
	if s.ConsecutiveUnknowns() != 5 {
		t.Fatalf("consecutive unknowns = %d, want 5", s.ConsecutiveUnknowns())
	}

	// The earlier failure streak survives the unknown run.
	if got, _ := s.Observe(models.StatusCritical); got != models.StatusCritical {
		t.Fatalf("third real failure should trip: got %s", got)
	}
}

func TestDegradedIsImmediate(t *testing.T) {
	s := healthyState(t, 3, 2)
	if got, _ := s.Observe(models.StatusDegraded); got != models.StatusDegraded {
		t.Fatalf("degraded should apply immediately, got %s", got)
	}
}

// ------------------------------------------------- cold start and unknowns

// TestColdStartIsUnknownNotHealthy guards the restart-during-an-outage case.
// A target nothing has probed yet must not claim health, or a checker that
// restarts mid-incident reports everything fine and the next reconcile pushes
// traffic into a region it has never observed.
func TestColdStartIsUnknownNotHealthy(t *testing.T) {
	s := NewState(3, 2)
	if got := s.Current(); got != models.StatusUnknown {
		t.Fatalf("fresh state is %s, want unknown", got)
	}
	if !s.IsInconclusive(3) {
		t.Error("a never-probed target must count as inconclusive for quorum")
	}
}

func TestHealthMustBeEarnedAfterColdStart(t *testing.T) {
	s := NewState(3, 2)

	if got, _ := s.Observe(models.StatusHealthy); got != models.StatusUnknown {
		t.Fatalf("one good probe after cold start gave %s, want still unknown", got)
	}
	if got, _ := s.Observe(models.StatusHealthy); got != models.StatusHealthy {
		t.Fatalf("two good probes should establish health, got %s", got)
	}
	if s.IsInconclusive(3) {
		t.Error("an established-healthy target must not count against quorum")
	}
}

// TestColdStartFailuresDoNotFakeDegraded checks that the pre-trip "show
// degraded so the operator sees the wobble" rule only applies to a target that
// was actually healthy. From unknown, an untripped failure streak is still
// unknown — we have no verdict to degrade from.
func TestColdStartFailuresDoNotFakeDegraded(t *testing.T) {
	s := NewState(3, 2)
	for i := 0; i < 2; i++ {
		if got, _ := s.Observe(models.StatusCritical); got != models.StatusUnknown {
			t.Fatalf("failure %d from cold start gave %s, want unknown", i+1, got)
		}
	}
	if got, _ := s.Observe(models.StatusCritical); got != models.StatusCritical {
		t.Fatalf("three failures should still trip from cold start, got %s", got)
	}
}

// TestUnknownsAreDebounced is the counterpart to the failure hysteresis: a
// single inconclusive probe must not make a target count against quorum, or a
// one-tick network blip on the checker swings the unknown ratio on its own.
func TestUnknownsAreDebounced(t *testing.T) {
	s := healthyState(t, 3, 2)

	for i := 1; i < 3; i++ {
		s.Observe(models.StatusUnknown)
		if s.IsInconclusive(3) {
			t.Fatalf("%d unknown(s) already counted against quorum; threshold is 3", i)
		}
	}
	s.Observe(models.StatusUnknown)
	if !s.IsInconclusive(3) {
		t.Error("3 consecutive unknowns should count against quorum")
	}

	// One conclusive probe clears the streak.
	s.Observe(models.StatusHealthy)
	if s.IsInconclusive(3) {
		t.Error("a conclusive probe should reset the unknown streak")
	}
}

func TestIsInconclusiveTreatsZeroThresholdAsOne(t *testing.T) {
	s := healthyState(t, 3, 2)
	s.Observe(models.StatusUnknown)
	if !s.IsInconclusive(0) {
		t.Error("threshold 0 should clamp to 1, not disable the check")
	}
}

func TestResetReturnsToUnknown(t *testing.T) {
	s := healthyState(t, 3, 2)
	s.Reset()
	if got := s.Current(); got != models.StatusUnknown {
		t.Fatalf("after reset state is %s, want unknown", got)
	}
}
