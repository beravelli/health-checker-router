package prober

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beravelli/health-checker-router/internal/models"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// TestLatencyDisabledByDefault is the safety default: an unconfigured policy
// never downgrades anything, however slow the response.
func TestLatencyDisabledByDefault(t *testing.T) {
	var p LatencyPolicy
	if p.Enabled() {
		t.Error("zero-value policy should be disabled")
	}
	for _, d := range []time.Duration{ms(1), ms(500), 30 * time.Second} {
		if p.Degraded(d) {
			t.Errorf("disabled policy degraded a %s response", d)
		}
	}
}

func TestLatencyThresholdWithoutBaseline(t *testing.T) {
	p := LatencyPolicy{DegradedAboveMs: 100}

	cases := []struct {
		elapsed time.Duration
		want    bool
	}{
		{ms(50), false},
		{ms(100), false}, // at the threshold, not above it
		{ms(101), true},
		{ms(900), true},
	}
	for _, tc := range cases {
		if got := p.Degraded(tc.elapsed); got != tc.want {
			t.Errorf("Degraded(%s) = %v, want %v", tc.elapsed, got, tc.want)
		}
	}
}

// TestBaselineAbsorbsPathLatency is the case this option exists to make safe.
//
// A threshold of 100ms calibrated while probing in-region would fire on every
// single sample once the probe moves to another region and each measurement
// carries ~70ms of RTT — shedding traffic from a service that is behaving
// perfectly. The baseline subtracts the path so the threshold measures the
// service.
func TestBaselineAbsorbsPathLatency(t *testing.T) {
	const pathRTT = 70

	inRegion := LatencyPolicy{DegradedAboveMs: 100}
	crossRegion := LatencyPolicy{DegradedAboveMs: 100, BaselineMs: pathRTT}

	// The service itself takes 40ms. Healthy by any reasonable standard.
	serviceTime := 40
	observed := ms(pathRTT + serviceTime)

	if !inRegion.Degraded(observed) {
		t.Error("precondition: an unbaselined threshold should misfire cross-region")
	}
	if crossRegion.Degraded(observed) {
		t.Error("baseline should absorb path RTT and leave this healthy")
	}

	// The service genuinely degrades to 150ms. Still caught.
	if !crossRegion.Degraded(ms(pathRTT + 150)) {
		t.Error("a genuinely slow service should still be caught through the baseline")
	}
}

// TestBaselineHigherThanElapsedClampsToZero: a sample faster than the
// configured baseline means the baseline is wrong, not that time ran backwards.
func TestBaselineHigherThanElapsedClampsToZero(t *testing.T) {
	p := LatencyPolicy{DegradedAboveMs: 50, BaselineMs: 200}

	if got := p.ServiceTime(ms(10)); got != 0 {
		t.Errorf("ServiceTime = %s, want 0", got)
	}
	if p.Degraded(ms(10)) {
		t.Error("a sub-baseline sample must not degrade")
	}
}

// TestBaselineWithoutThresholdStaysDisabled: setting only a baseline is a
// half-configuration, and it must not silently enable anything.
func TestBaselineWithoutThresholdStaysDisabled(t *testing.T) {
	p := LatencyPolicy{BaselineMs: 70}
	if p.Enabled() || p.Degraded(10*time.Second) {
		t.Error("a baseline alone should not enable latency degradation")
	}
}

func TestDescribeNamesTheBaseline(t *testing.T) {
	// Without a baseline the message is simple.
	plain := LatencyPolicy{DegradedAboveMs: 100}.Describe(ms(250))
	if !strings.Contains(plain, "250ms") || !strings.Contains(plain, "100ms") {
		t.Errorf("describe = %q; should name the measurement and threshold", plain)
	}

	// With one, an operator needs to see both numbers or "degraded" is
	// unexplainable against a dashboard showing 250ms.
	withBase := LatencyPolicy{DegradedAboveMs: 100, BaselineMs: 70}.Describe(ms(250))
	for _, want := range []string{"250ms", "180ms", "70ms", "100ms"} {
		if !strings.Contains(withBase, want) {
			t.Errorf("describe = %q; missing %s", withBase, want)
		}
	}
}

// ------------------------------------------------------- through the probes

func TestHTTPProbeAppliesLatencyPolicy(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	t.Run("disabled stays healthy", func(t *testing.T) {
		p := &HTTPProbe{URL: slow.URL, Client: slow.Client()}
		st, _, _, err := p.Run(context.Background())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if st != models.StatusHealthy {
			t.Errorf("status = %s, want healthy with latency policy off", st)
		}
	})

	t.Run("enabled degrades", func(t *testing.T) {
		p := &HTTPProbe{URL: slow.URL, Client: slow.Client(),
			Latency: LatencyPolicy{DegradedAboveMs: 20}}
		st, _, detail, err := p.Run(context.Background())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if st != models.StatusDegraded {
			t.Fatalf("status = %s, want degraded", st)
		}
		if !strings.Contains(detail, "slow") {
			t.Errorf("detail = %q; should explain the downgrade", detail)
		}
	})

	t.Run("baseline absorbs it", func(t *testing.T) {
		p := &HTTPProbe{URL: slow.URL, Client: slow.Client(),
			Latency: LatencyPolicy{DegradedAboveMs: 20, BaselineMs: 500}}
		st, _, _, err := p.Run(context.Background())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if st != models.StatusHealthy {
			t.Errorf("status = %s, want healthy — the baseline covers this", st)
		}
	})
}

// TestLatencyNeverOverridesAFailure: a non-2xx is critical regardless of how
// fast it came back. Latency only ever downgrades an otherwise healthy result.
func TestLatencyNeverOverridesAFailure(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	p := &HTTPProbe{URL: broken.URL, Client: broken.Client(),
		Latency: LatencyPolicy{DegradedAboveMs: 1}}
	st, _, _, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st != models.StatusCritical {
		t.Errorf("status = %s, want critical — a fast 500 is still a 500", st)
	}
}

func TestLatencyPolicyReadFromConfig(t *testing.T) {
	got := latencyPolicy(map[string]any{
		"degradedAboveMs":   float64(250),
		"latencyBaselineMs": float64(70),
	})
	if got.DegradedAboveMs != 250 || got.BaselineMs != 70 {
		t.Errorf("policy = %+v, want {250 70}", got)
	}

	// Absent config disables it.
	if empty := latencyPolicy(map[string]any{}); empty.Enabled() {
		t.Error("empty probe config should leave latency degradation off")
	}
}
