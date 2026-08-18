// Package obs holds the Prometheus instrumentation.
//
// A system that autonomously moves production traffic has to be answerable
// after the fact: which region scored what, when weights changed, when quorum
// failed, and — most importantly — whether the checker was awake at all.
//
// The heartbeat is the load-bearing metric here. Every other signal in this
// system is emitted by a process that is working. A checker isolated from the
// network cannot page, cannot write to Postgres, and cannot serve /readyz, so
// the only observable difference between "nothing is wrong" and "the thing that
// tells you something is wrong is dead" is the absence of a heartbeat. That has
// to be alerted on externally, by something watching for silence:
//
//	# The checker has not completed an evaluation cycle in 3 minutes.
//	- alert: HealthCheckerStalled
//	  expr: time() - hcr_last_evaluation_timestamp_seconds > 180
//	  labels: { severity: critical }
//
//	# Nothing is scraping-visible at all: no leader anywhere in the fleet.
//	- alert: HealthCheckerNoLeader
//	  expr: max(hcr_leader) == 0 or absent(hcr_leader)
//	  for: 2m
//	  labels: { severity: critical }
package obs

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// LeaderState reports whether this replica currently drives the pipeline.
type LeaderState interface {
	IsLeader() bool
	Term() int64
}

// Metrics is the instrument set, plus the registry they live in.
//
// A private registry rather than the default one: the default is global mutable
// state that any dependency can write to, and tests that build two Metrics
// would panic on duplicate registration.
type Metrics struct {
	registry *prometheus.Registry

	// Pipeline liveness.
	lastEvaluation prometheus.Gauge
	evaluations    *prometheus.CounterVec
	evaluationTime prometheus.Histogram

	// Health signal.
	regionScore  *prometheus.GaugeVec
	targetStatus *prometheus.GaugeVec
	unknownRatio *prometheus.GaugeVec

	// Probes.
	probeTotal    *prometheus.CounterVec
	probeDuration *prometheus.HistogramVec

	// Decisions.
	regionWeight   *prometheus.GaugeVec
	weightChanges  *prometheus.CounterVec
	failOpenActive prometheus.Gauge
	failOpenTotal  prometheus.Counter
	regionTrusted  *prometheus.GaugeVec
}

// New builds the instrument set and registers it.
func New(leader LeaderState) *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		lastEvaluation: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hcr_last_evaluation_timestamp_seconds",
			Help: "Unix timestamp of the last completed evaluation cycle. Alert on staleness — this is the dead man's switch.",
		}),
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hcr_evaluations_total",
			Help: "Evaluation cycles by outcome.",
		}, []string{"outcome"}),
		evaluationTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "hcr_evaluation_duration_seconds",
			Help:    "Wall time of an evaluation cycle.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}),

		regionScore: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hcr_region_score",
			Help: "Composite region health score, 0 to 100.",
		}, []string{"region"}),
		targetStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hcr_target_status",
			Help: "Debounced target status as an ordinal: 0 healthy, 1 degraded, 2 critical, 3 unknown.",
		}, []string{"service", "region"}),
		unknownRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hcr_region_unknown_ratio",
			Help: "Fraction of a region's targets whose signal is inconclusive.",
		}, []string{"region"}),

		probeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hcr_probes_total",
			Help: "Probes executed, by observed status.",
		}, []string{"service", "region", "status"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hcr_probe_duration_seconds",
			Help:    "Probe latency.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"service", "region"}),

		regionWeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hcr_region_weight",
			Help: "Load-balancer weight currently applied to a region.",
		}, []string{"region"}),
		weightChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hcr_weight_changes_total",
			Help: "Weight changes pushed to the routing provider, by outcome.",
		}, []string{"provider", "outcome"}),
		failOpenActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hcr_fail_open_active",
			Help: "1 while last-known-good weights are being held because the signal is inconclusive.",
		}),
		failOpenTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hcr_fail_open_windows_total",
			Help: "Fail-open windows opened.",
		}),
		regionTrusted: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hcr_region_trusted",
			Help: "1 when a region's health signal met quorum and may drive weights.",
		}, []string{"region"}),
	}

	reg.MustRegister(
		m.lastEvaluation, m.evaluations, m.evaluationTime,
		m.regionScore, m.targetStatus, m.unknownRatio,
		m.probeTotal, m.probeDuration,
		m.regionWeight, m.weightChanges, m.failOpenActive, m.failOpenTotal, m.regionTrusted,
	)

	// Leadership is read from the elector at scrape time rather than pushed, so
	// it cannot go stale between transitions.
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "hcr_leader",
		Help: "1 on the replica that holds the advisory lock and drives the pipeline.",
	}, func() float64 {
		if leader != nil && leader.IsLeader() {
			return 1
		}
		return 0
	}))
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "hcr_leader_terms_total",
		Help: "Leadership terms this replica has held. Climbing steadily means the lock is flapping.",
	}, func() float64 {
		if leader == nil {
			return 0
		}
		return float64(leader.Term())
	}))

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler serves the metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry, for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ----------------------------------------------------------- recording

// EvaluationComplete records a finished cycle. outcome is "applied", "held" or
// "error".
//
// Called on every cycle including failures, because the heartbeat has to
// distinguish "still running but erroring" from "not running", and those want
// different alerts.
func (m *Metrics) EvaluationComplete(outcome string, d time.Duration) {
	m.lastEvaluation.SetToCurrentTime()
	m.evaluations.WithLabelValues(outcome).Inc()
	m.evaluationTime.Observe(d.Seconds())
}

// RegionScore records a region's composite score.
func (m *Metrics) RegionScore(region string, score float64) {
	m.regionScore.WithLabelValues(region).Set(score)
}

// TargetStatus records a target's debounced status.
func (m *Metrics) TargetStatus(service, region string, ordinal float64) {
	m.targetStatus.WithLabelValues(service, region).Set(ordinal)
}

// UnknownRatio records how blind a region is.
func (m *Metrics) UnknownRatio(region string, ratio float64) {
	m.unknownRatio.WithLabelValues(region).Set(ratio)
}

// ProbeObserved records one probe execution.
func (m *Metrics) ProbeObserved(service, region, status string, d time.Duration) {
	m.probeTotal.WithLabelValues(service, region, status).Inc()
	m.probeDuration.WithLabelValues(service, region).Observe(d.Seconds())
}

// RegionWeight records the weight applied to a region.
func (m *Metrics) RegionWeight(region string, weight float64) {
	m.regionWeight.WithLabelValues(region).Set(weight)
}

// WeightChange records a push to the routing provider.
func (m *Metrics) WeightChange(provider, outcome string) {
	m.weightChanges.WithLabelValues(provider, outcome).Inc()
}

// RegionTrusted records whether a region's signal met quorum.
func (m *Metrics) RegionTrusted(region string, trusted bool) {
	v := 0.0
	if trusted {
		v = 1
	}
	m.regionTrusted.WithLabelValues(region).Set(v)
}

// FailOpen records the state of the fail-open window.
func (m *Metrics) FailOpen(active bool) {
	if active {
		m.failOpenActive.Set(1)
		return
	}
	m.failOpenActive.Set(0)
}

// FailOpenOpened records a newly opened window.
func (m *Metrics) FailOpenOpened() { m.failOpenTotal.Inc() }
