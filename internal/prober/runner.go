package prober

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Recorder persists probe observations.
type Recorder interface {
	RecordProbe(ctx context.Context, r models.ProbeResult) error
}

// Notifier is told whenever a target's debounced status changes.
type Notifier interface {
	OnStatusChange(serviceID, regionID string, from, to models.Status)
}

// Observer receives every raw probe outcome, debounced or not.
//
// Separate from Notifier, which only fires on transitions: metrics need the
// full stream, including the unknowns that hysteresis deliberately swallows,
// because a rising unknown rate is the earliest signal that the checker itself
// is in trouble. An interface rather than a direct dependency so this package
// stays free of the metrics library.
type Observer interface {
	ProbeObserved(service, region, status string, d time.Duration)
	TargetStatus(service, region string, ordinal float64)
}

// TargetHealthChecker resolves the health of an AWS load-balancer target
// group. It is an interface so this package does not import the AWS SDK; the
// concrete implementation lives in internal/awsauth.
type TargetHealthChecker interface {
	TargetGroupHealth(ctx context.Context, svc models.Service, region string) (models.Status, string, error)
}

// albProbe adapts a TargetHealthChecker to the Probe interface.
type albProbe struct {
	checker TargetHealthChecker
	svc     models.Service
	region  string
}

func (p *albProbe) Run(ctx context.Context) (models.Status, time.Duration, string, error) {
	start := time.Now()
	st, detail, err := p.checker.TargetGroupHealth(ctx, p.svc, p.region)
	elapsed := time.Since(start)
	if err != nil {
		// Could not query AWS. That says nothing about the target's health.
		return models.StatusUnknown, elapsed, err.Error(), nil
	}
	return st, elapsed, detail, nil
}

// Runner owns one goroutine per target and keeps their hysteresis state.
type Runner struct {
	cfg      *config.Config
	recorder Recorder
	notifier Notifier
	observer Observer
	aws      TargetHealthChecker
	log      *slog.Logger

	mu      sync.RWMutex
	states  map[string]*State
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// NewRunner constructs a Runner. aws may be nil if no ALB/NLB targets are
// registered; such targets then report unknown rather than crashing.
func NewRunner(cfg *config.Config, rec Recorder, notifier Notifier, observer Observer, aws TargetHealthChecker, log *slog.Logger) *Runner {
	return &Runner{
		cfg:      cfg,
		recorder: rec,
		notifier: notifier,
		observer: observer,
		aws:      aws,
		log:      log,
		states:   map[string]*State{},
		cancels:  map[string]context.CancelFunc{},
	}
}

// Sync reconciles the running probe goroutines with the registered services:
// it starts goroutines for new targets and stops those that were removed.
//
// Called on startup and whenever Admin mutates the service list, so a newly
// registered service begins probing without a restart.
func (r *Runner) Sync(ctx context.Context, services []models.Service) {
	desired := map[string]Target{}

	for _, svc := range services {
		for _, sr := range svc.Regions {
			t, err := r.buildTarget(svc, sr)
			if err != nil {
				// Fail closed for this target only: log and skip, rather than
				// probing with a broken configuration.
				r.log.Error("skipping target with invalid configuration",
					"service", svc.Name, "region", sr.RegionID, "error", err)
				continue
			}
			desired[t.Key()] = t
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Stop targets that no longer exist.
	for key, cancel := range r.cancels {
		if _, ok := desired[key]; !ok {
			cancel()
			delete(r.cancels, key)
			delete(r.states, key)
			r.log.Info("stopped probe target", "target", key)
		}
	}

	// Start targets that are new.
	for key, t := range desired {
		if _, ok := r.cancels[key]; ok {
			continue
		}
		tctx, cancel := context.WithCancel(ctx)
		r.cancels[key] = cancel
		r.states[key] = NewState(
			orDefault(t.failuresToTrip, r.cfg.FailuresToTrip),
			orDefault(t.successToRecover, r.cfg.SuccessToRecover),
		)
		r.wg.Add(1)
		go func(t Target) {
			defer r.wg.Done()
			r.loop(tctx, t)
		}(t)
		r.log.Info("started probe target", "target", key, "interval", t.Interval)
	}
}

// Stop cancels every probe goroutine and waits for them to exit.
func (r *Runner) Stop() {
	r.mu.Lock()
	for key, cancel := range r.cancels {
		cancel()
		delete(r.cancels, key)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// Status returns the debounced status for a target, and whether it is tracked.
func (r *Runner) Status(serviceID, regionID string) (models.Status, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.states[serviceID+"|"+regionID]
	if !ok {
		return models.StatusUnknown, false
	}
	return s.Current(), true
}

// UnknownRatio reports the fraction of tracked targets in a region whose signal
// is currently unusable. The fail-open controller uses it to decide whether the
// region's health can be trusted at all.
//
// A target counts once it has returned cfg.UnknownsToDistrust consecutive
// inconclusive probes, or while it still sits at its seeded unknown. Counting a
// single unknown would make this the one undebounced path in the pipeline, and
// a checker-side network fault produces exactly that: every target going
// inconclusive on the same tick.
func (r *Runner) UnknownRatio(regionID string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	threshold := r.cfg.UnknownsToDistrust
	var total, unknown int
	for key, st := range r.states {
		if !strings.HasSuffix(key, "|"+regionID) {
			continue
		}
		total++
		if st.IsInconclusive(threshold) {
			unknown++
		}
	}
	if total == 0 {
		// No targets registered for the region. There is nothing to be blind
		// about, so report full visibility rather than tripping fail-open on an
		// empty region.
		return 0
	}
	return float64(unknown) / float64(total)
}

// loop probes one target until its context is cancelled.
func (r *Runner) loop(ctx context.Context, t Target) {
	// Stagger start times so N targets on the same interval do not all fire on
	// the same tick and stampede the network.
	jitter := time.Duration(rand.Int63n(int64(t.Interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()

	for {
		r.probeOnce(ctx, t)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) probeOnce(ctx context.Context, t Target) {
	pctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	observed, latency, detail, err := t.Probe.Run(pctx)
	if err != nil {
		observed = models.StatusUnknown
		detail = err.Error()
		r.log.Warn("probe could not run", "target", t.Key(), "error", err)
	}

	r.mu.RLock()
	state := r.states[t.Key()]
	r.mu.RUnlock()
	if state == nil {
		return
	}

	before := state.Current()
	after, changed := state.Observe(observed)

	// Persist the raw observation, not the debounced one, so the uptime trend
	// reflects what actually happened rather than what we chose to act on.
	result := models.ProbeResult{
		ServiceID: t.ServiceID,
		RegionID:  t.RegionID,
		Status:    observed,
		LatencyMs: int(latency.Milliseconds()),
		Detail:    detail,
		ProbedAt:  time.Now().UTC(),
	}
	// Use a detached context so a cancelled probe still records its result.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer writeCancel()
	if err := r.recorder.RecordProbe(writeCtx, result); err != nil {
		r.log.Error("record probe", "target", t.Key(), "error", err)
	}

	if r.observer != nil {
		r.observer.ProbeObserved(t.Service, t.RegionID, string(observed), latency)
		r.observer.TargetStatus(t.Service, t.RegionID, statusOrdinal(after))
	}

	if changed && r.notifier != nil {
		r.log.Info("target status changed",
			"target", t.Key(), "from", before, "to", after, "detail", detail)
		r.notifier.OnStatusChange(t.ServiceID, t.RegionID, before, after)
	}
}

// statusOrdinal maps a status onto the numeric scale hcr_target_status exports.
// Ordered by severity so a max() across a region is meaningful, with unknown
// highest because "we cannot tell" is the state that most needs attention.
func statusOrdinal(s models.Status) float64 {
	switch s {
	case models.StatusHealthy:
		return 0
	case models.StatusDegraded:
		return 1
	case models.StatusCritical:
		return 2
	default:
		return 3
	}
}

// buildTarget turns a stored service definition into a runnable target.
func (r *Runner) buildTarget(svc models.Service, sr models.ServiceRegion) (Target, error) {
	interval := time.Duration(svc.ProbeIntervalS) * time.Second
	if interval <= 0 {
		interval = r.cfg.ProbeInterval
	}
	timeout := time.Duration(svc.ProbeTimeoutS) * time.Second
	if timeout <= 0 || timeout > interval {
		timeout = min(r.cfg.ProbeTimeout, interval)
	}

	t := Target{
		ServiceID:        svc.ID,
		Service:          svc.Name,
		RegionID:         sr.RegionID,
		Endpoint:         sr.Endpoint,
		Interval:         interval,
		Timeout:          timeout,
		failuresToTrip:   svc.ConsecutiveFailuresToTrip,
		successToRecover: svc.ConsecutiveSuccessToRecover,
	}

	switch svc.ProbeType {
	case models.ProbeHTTP:
		client, err := NewHTTPClient(timeout, svc.CACertSecretEnv)
		if err != nil {
			return t, err
		}
		url, err := httpURL(sr.Endpoint, svc.ProbeConfig)
		if err != nil {
			return t, err
		}
		t.Probe = &HTTPProbe{
			URL:             url,
			ExpectedStatus:  cfgInt(svc.ProbeConfig, "expectedStatus", http.StatusOK),
			Client:          client,
			DegradedAboveMs: cfgInt(svc.ProbeConfig, "degradedAboveMs", 0),
		}

	case models.ProbeTCP:
		port := cfgInt(svc.ProbeConfig, "port", 0)
		if port == 0 {
			return t, fmt.Errorf("tcp probe requires probeConfig.port")
		}
		host := hostOf(sr.Endpoint)
		if host == "" {
			return t, fmt.Errorf("tcp probe requires a region endpoint host")
		}
		t.Probe = &TCPProbe{Address: fmt.Sprintf("%s:%d", host, port)}

	case models.ProbeGRPC:
		client, err := NewHTTPClient(timeout, svc.CACertSecretEnv)
		if err != nil {
			return t, err
		}
		if sr.Endpoint == "" {
			return t, fmt.Errorf("grpc probe requires a region endpoint")
		}
		t.Probe = &GRPCProbe{
			Address: sr.Endpoint,
			Service: cfgString(svc.ProbeConfig, "grpcService", ""),
			Client:  client,
		}

	case models.ProbeALB, models.ProbeNLB:
		if r.aws == nil {
			return t, fmt.Errorf("%s probe requires AWS credentials to be configured", svc.ProbeType)
		}
		if cfgString(svc.ProbeConfig, "targetGroupArn", "") == "" {
			return t, fmt.Errorf("%s probe requires probeConfig.targetGroupArn", svc.ProbeType)
		}
		t.Probe = &albProbe{checker: r.aws, svc: svc, region: sr.RegionID}

	case models.ProbeCustom:
		path := cfgString(svc.ProbeConfig, "command", "")
		if path == "" {
			return t, fmt.Errorf("custom probe requires probeConfig.command")
		}
		t.Probe = &CustomProbe{
			Path:      path,
			Args:      cfgStrings(svc.ProbeConfig, "args"),
			Allowlist: r.cfg.BinaryAllowed,
		}

	default:
		return t, fmt.Errorf("unsupported probe type %q", svc.ProbeType)
	}

	return t, nil
}

// httpURL assembles the probe URL from the region endpoint and probe config.
func httpURL(endpoint string, cfg map[string]any) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("http probe requires a region endpoint")
	}
	base := strings.TrimSuffix(endpoint, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	path := cfgString(cfg, "path", "/healthz")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path, nil
}

func hostOf(endpoint string) string {
	e := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexAny(e, "/:"); i >= 0 {
		e = e[:i]
	}
	return e
}

func cfgInt(cfg map[string]any, key string, def int) int {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64: // JSON numbers decode as float64
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

func cfgString(cfg map[string]any, key, def string) string {
	if s, ok := cfg[key].(string); ok && s != "" {
		return s
	}
	return def
}

func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
