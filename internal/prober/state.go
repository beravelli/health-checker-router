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

// NewState returns a state machine seeded as unknown.
//
// Unknown, not healthy: a target that has never been probed has not earned a
// verdict. Seeding healthy means a checker restarted in the middle of an
// outage reports every target healthy with zero counters, and the next
// reconcile can push traffic back into a region nothing has actually observed.
// Health has to be established by SuccessToRecover consecutive good probes,
// exactly as it is after a trip.
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
		current:          models.StatusUnknown,
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
func (s *State) ConsecutiveUnknowns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consecutiveUnknowns
}

// IsInconclusive reports whether this target should count against its region's
// quorum, given how many consecutive unknowns are tolerated first.
//
// Two ways to qualify. Either the target has returned threshold inconclusive
// probes in a row — debouncing unknowns the way failuresToTrip debounces
// failures, so one transport blip does not swing the ratio — or it is still
// sitting at its seeded unknown, meaning no probe has ever produced a verdict
// for it. The second case is what stops a freshly restarted checker from
// reporting confident health it has not measured.
func (s *State) IsInconclusive(threshold int) bool {
	if threshold < 1 {
		threshold = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current == models.StatusUnknown || s.consecutiveUnknowns >= threshold
}

// Reset returns the machine to its unseeded state with cleared counters.
func (s *State) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutiveFailures = 0
	s.consecutiveSuccesses = 0
	s.consecutiveUnknowns = 0
	s.current = models.StatusUnknown
}
