package obs

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeLeader struct {
	leader bool
	term   int64
}

func (f *fakeLeader) IsLeader() bool { return f.leader }
func (f *fakeLeader) Term() int64    { return f.term }

// TestHeartbeatAdvances covers the dead man's switch. The alert compares
// time() against this gauge, so a cycle that does not update it is
// indistinguishable from a process that has stopped.
func TestHeartbeatAdvances(t *testing.T) {
	m := New(&fakeLeader{})

	before := testutil.ToFloat64(m.lastEvaluation)
	if before != 0 {
		t.Fatalf("heartbeat starts at %v, want 0 before the first cycle", before)
	}

	m.EvaluationComplete("applied", 10*time.Millisecond)
	after := testutil.ToFloat64(m.lastEvaluation)

	if after <= 0 {
		t.Fatal("heartbeat did not advance after a completed cycle")
	}
	if delta := time.Since(time.Unix(int64(after), 0)); delta > time.Minute {
		t.Errorf("heartbeat is %s stale immediately after being set", delta)
	}
}

// TestHeartbeatAdvancesOnErrorCycles: a loop that is running but failing must
// still beat, or staleness alerts fire for the wrong reason and the separate
// error-rate alert never gets a chance to say what is actually wrong.
func TestHeartbeatAdvancesOnErrorCycles(t *testing.T) {
	m := New(&fakeLeader{})
	m.EvaluationComplete("error", time.Millisecond)

	if testutil.ToFloat64(m.lastEvaluation) <= 0 {
		t.Error("an erroring cycle must still update the heartbeat")
	}
	if got := testutil.ToFloat64(m.evaluations.WithLabelValues("error")); got != 1 {
		t.Errorf("error counter = %v, want 1", got)
	}
}

func TestLeaderGaugeReflectsElector(t *testing.T) {
	l := &fakeLeader{}
	m := New(l)

	body := gather(t, m)
	if !strings.Contains(body, "hcr_leader 0") {
		t.Error("standby replica should export hcr_leader 0")
	}

	// Read at scrape time, so a transition needs no explicit push.
	l.leader = true
	l.term = 2
	body = gather(t, m)
	if !strings.Contains(body, "hcr_leader 1") {
		t.Error("leader should export hcr_leader 1")
	}
	if !strings.Contains(body, "hcr_leader_terms_total 2") {
		t.Error("term counter should follow the elector")
	}
}

func TestFailOpenGaugeToggles(t *testing.T) {
	m := New(&fakeLeader{})

	m.FailOpen(true)
	if got := testutil.ToFloat64(m.failOpenActive); got != 1 {
		t.Errorf("fail-open gauge = %v, want 1", got)
	}
	m.FailOpen(false)
	if got := testutil.ToFloat64(m.failOpenActive); got != 0 {
		t.Errorf("fail-open gauge = %v, want 0", got)
	}
}

func TestRegionMetricsAreLabelled(t *testing.T) {
	m := New(&fakeLeader{})
	m.RegionScore("us-east-1", 87.5)
	m.UnknownRatio("us-east-1", 0.25)
	m.RegionTrusted("us-east-1", true)
	m.RegionTrusted("us-west-2", false)
	m.RegionWeight("us-east-1", 70)

	body := gather(t, m)
	for _, want := range []string{
		`hcr_region_score{region="us-east-1"} 87.5`,
		`hcr_region_unknown_ratio{region="us-east-1"} 0.25`,
		`hcr_region_trusted{region="us-east-1"} 1`,
		`hcr_region_trusted{region="us-west-2"} 0`,
		`hcr_region_weight{region="us-east-1"} 70`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in exposition", want)
		}
	}
}

func TestProbeAndTargetMetrics(t *testing.T) {
	m := New(&fakeLeader{})
	m.ProbeObserved("api", "us-east-1", "healthy", 25*time.Millisecond)
	m.ProbeObserved("api", "us-east-1", "unknown", 3*time.Second)
	m.TargetStatus("api", "us-east-1", 3)

	body := gather(t, m)
	for _, want := range []string{
		`hcr_probes_total{region="us-east-1",service="api",status="healthy"} 1`,
		`hcr_probes_total{region="us-east-1",service="api",status="unknown"} 1`,
		`hcr_target_status{region="us-east-1",service="api"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in exposition", want)
		}
	}
}

func TestWeightChangeOutcomes(t *testing.T) {
	m := New(&fakeLeader{})
	m.WeightChange("aws_native", "error")
	m.WeightChange("aws_native", "error")
	m.WeightChange("aws_native", "success")

	if got := testutil.ToFloat64(m.weightChanges.WithLabelValues("aws_native", "error")); got != 2 {
		t.Errorf("error count = %v, want 2", got)
	}
}

// TestTwoMetricsSetsCoexist: New uses a private registry rather than the global
// default, so building more than one — as tests do — must not panic on
// duplicate registration.
func TestTwoMetricsSetsCoexist(t *testing.T) {
	_ = New(&fakeLeader{})
	_ = New(&fakeLeader{})
}

// TestNilLeaderIsSafe covers a headless replica built without an elector.
func TestNilLeaderIsSafe(t *testing.T) {
	m := New(nil)
	if body := gather(t, m); !strings.Contains(body, "hcr_leader 0") {
		t.Error("a nil elector should export hcr_leader 0 rather than panicking")
	}
}

// gather renders the registry in Prometheus text format.
func gather(t *testing.T, m *Metrics) string {
	t.Helper()
	var sb strings.Builder
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			sb.WriteString(f.GetName())
			if labels := metric.GetLabel(); len(labels) > 0 {
				sb.WriteString("{")
				for i, l := range labels {
					if i > 0 {
						sb.WriteString(",")
					}
					sb.WriteString(l.GetName() + `="` + l.GetValue() + `"`)
				}
				sb.WriteString("}")
			}
			switch {
			case metric.GetGauge() != nil:
				sb.WriteString(" " + trimFloat(metric.GetGauge().GetValue()))
			case metric.GetCounter() != nil:
				sb.WriteString(" " + trimFloat(metric.GetCounter().GetValue()))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
