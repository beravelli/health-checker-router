package prober

import (
	"sync"

	"github.com/beravelli/health-checker-router/internal/models"
)

// State is the hysteresis state machine for one (service, region) target.
//
// It exists to solve the flapping-probe edge case: a single failed probe must
// not move traffic. A target only trips after failuresToTrip consecutive
// failures, and only recovers after successToRecover consecutive successes.
// Anything in between leaves the reported status unchanged.
//
// Unknown results are handled separately: they do not reset either counter,
// because "we could not tell" is not evidence of health or of failure. They are
// surfaced to the fail-open controller instead.
type State struct {
	mu sync.Mutex

	failuresToTrip   int
	successToRecover int

	consecutiveFailures  int
	consecutiveSuccesses int
	consecutiveUnknowns  int

	current models.Status
}

// NewState returns a state machine seeded as healthy.
func NewState(failuresToTrip, successToRecover int) *State {
	if failuresToTrip < 1 {
		failuresToTrip = 1
	}
	if successToRecover < 1 {
		successToRecover = 1
	}
	return &State{
		failuresToTrip:   failuresToTrip,
		successToRecover: successToRecover,
		current:          models.StatusHealthy,
	}
}

// Observe feeds one raw probe outcome into the machine and returns the status
// the target should now be reported as, plus whether that status changed.
func (s *State) Observe(observed models.Status) (models.Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.current

	switch observed {
	case models.StatusHealthy:
		s.consecutiveFailures = 0
		s.consecutiveUnknowns = 0
		s.consecutiveSuccesses++
		if s.current != models.StatusHealthy && s.consecutiveSuccesses >= s.successToRecover {
			s.current = models.StatusHealthy
		}

	case models.StatusCritical:
		s.consecutiveSuccesses = 0
		s.consecutiveUnknowns = 0
		s.consecutiveFailures++
		if s.consecutiveFailures >= s.failuresToTrip {
			s.current = models.StatusCritical
		} else if s.current == models.StatusHealthy {
			// Failing but not yet tripped: show degraded so the operator sees
			// the wobble before it becomes a routing change.
			s.current = models.StatusDegraded
		}

	case models.StatusDegraded:
		// Degradation is reported immediately — it does not move traffic to
		// zero, it only reduces the composite score — but it does reset the
		// recovery counter.
		s.consecutiveSuccesses = 0
		s.consecutiveUnknowns = 0
		s.current = models.StatusDegraded

	case models.StatusUnknown:
		// Deliberately does not touch failure/success counters.
		s.consecutiveUnknowns++
	}

	return s.current, s.current != prev
}

// Current returns the debounced status without observing anything.
func (s *State) Current() models.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// ConsecutiveUnknowns reports how many inconclusive probes have run in a row.
// The fail-open controller uses this to decide when a signal is unusable.
func (s *State) ConsecutiveUnknowns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consecutiveUnknowns
}

// Reset returns the machine to healthy with cleared counters.
func (s *State) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutiveFailures = 0
	s.consecutiveSuccesses = 0
	s.consecutiveUnknowns = 0
	s.current = models.StatusHealthy
}
