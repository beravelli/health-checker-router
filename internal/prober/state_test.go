package prober

import (
	"testing"

	"github.com/beravelli/health-checker-router/internal/models"
)

func TestFlappingDoesNotTrip(t *testing.T) {
	// The flapping edge case: alternating pass/fail near a threshold must never
	// reach the tripped state, because the failure counter resets on each pass.
	s := NewState(3, 2)
	for i := 0; i < 20; i++ {
		s.Observe(models.StatusCritical)
		s.Observe(models.StatusHealthy)
	}
	if got := s.Current(); got == models.StatusCritical {
		t.Fatalf("flapping tripped the target to critical; want never critical")
	}
}

func TestTripsAfterThreeConsecutiveFailures(t *testing.T) {
	s := NewState(3, 2)

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
	s := NewState(3, 2)
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
	s := NewState(3, 2)
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
	s := NewState(3, 2)
	if got, _ := s.Observe(models.StatusDegraded); got != models.StatusDegraded {
		t.Fatalf("degraded should apply immediately, got %s", got)
	}
}
