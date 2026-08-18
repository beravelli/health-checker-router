package prober

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Probe performs one health check against a target.
type Probe interface {
	// Run returns the observed status, the latency, and a human-readable
	// detail line. An error means the probe could not be executed at all,
	// which the caller maps to StatusUnknown, not StatusCritical.
	Run(ctx context.Context) (models.Status, time.Duration, string, error)
}

// LatencyPolicy decides whether a successful-but-slow response counts as
// degraded.
//
// Opt-in, and off by default. Latency is always recorded — on the probe result
// and in hcr_probe_duration_seconds — but only influences a target's *status*,
// and therefore traffic, when DegradedAboveMs is set.
//
// BaselineMs is what makes the option safe from any vantage point. Elapsed time
// measures the service plus the network path to it, and the path is a property
// of where the probe runs rather than of the service's health. Probing
// us-east-1 from another region adds tens of milliseconds of RTT to every
// sample; without a baseline, a threshold calibrated in-region fires
// permanently and sheds traffic from a service that is behaving perfectly.
//
// Set BaselineMs to the known network floor between this probe and this region
// — the p50 of a trivial request, or what a dedicated latency agent reports as
// the path minimum. The comparison then runs against time attributable to the
// service.
type LatencyPolicy struct {
	// DegradedAboveMs is the threshold above the baseline. Zero disables
	// latency-based degradation entirely.
	DegradedAboveMs int
	// BaselineMs is subtracted from every measurement before comparison.
	BaselineMs int
}

// Enabled reports whether this policy can produce a degraded status.
func (p LatencyPolicy) Enabled() bool { return p.DegradedAboveMs > 0 }

// ServiceTime returns the portion of elapsed attributable to the service,
// never negative — a sample faster than the configured baseline means the
// baseline is too high, not that the service took negative time.
func (p LatencyPolicy) ServiceTime(elapsed time.Duration) time.Duration {
	adjusted := elapsed - time.Duration(p.BaselineMs)*time.Millisecond
	if adjusted < 0 {
		return 0
	}
	return adjusted
}

// Degraded reports whether elapsed should downgrade an otherwise healthy
// response.
func (p LatencyPolicy) Degraded(elapsed time.Duration) bool {
	if !p.Enabled() {
		return false
	}
	return p.ServiceTime(elapsed) > time.Duration(p.DegradedAboveMs)*time.Millisecond
}

// Describe renders the policy for a probe detail line, so an operator seeing
// "degraded" can tell what it was measured against.
func (p LatencyPolicy) Describe(elapsed time.Duration) string {
	if p.BaselineMs <= 0 {
		return fmt.Sprintf("%dms, above the %dms threshold",
			elapsed.Milliseconds(), p.DegradedAboveMs)
	}
	return fmt.Sprintf("%dms total, %dms after the %dms baseline, above the %dms threshold",
		elapsed.Milliseconds(), p.ServiceTime(elapsed).Milliseconds(),
		p.BaselineMs, p.DegradedAboveMs)
}

// Target is one (service, region) pair to probe.
type Target struct {
	ServiceID string
	Service   string
	RegionID  string
	Endpoint  string
	Probe     Probe
	Interval  time.Duration
	Timeout   time.Duration

	// Per-service hysteresis overrides; zero means fall back to the global
	// defaults in config.
	failuresToTrip   int
	successToRecover int
}

// Key uniquely identifies a target.
func (t Target) Key() string { return t.ServiceID + "|" + t.RegionID }

// ---------------------------------------------------------------- HTTP

// HTTPProbe checks an HTTP(S) endpoint for an expected status code.
type HTTPProbe struct {
	URL            string
	ExpectedStatus int
	Client         *http.Client
	// Latency optionally downgrades a slow-but-successful response. Disabled
	// unless DegradedAboveMs is set.
	Latency LatencyPolicy
}

// Run issues the request and classifies the response.
func (p *HTTPProbe) Run(ctx context.Context) (models.Status, time.Duration, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return models.StatusUnknown, 0, "", fmt.Errorf("build request: %w", err)
	}

	start := time.Now()
	resp, err := p.Client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		// A transport error is ambiguous — the target may be down, or the
		// probe may be the thing that is broken. Report unknown and let
		// hysteresis plus fail-open decide.
		return models.StatusUnknown, elapsed, err.Error(), nil
	}
	defer func() { _ = resp.Body.Close() }()

	want := p.ExpectedStatus
	if want == 0 {
		want = http.StatusOK
	}
	if resp.StatusCode != want {
		return models.StatusCritical, elapsed,
			fmt.Sprintf("HTTP %d, want %d", resp.StatusCode, want), nil
	}
	if p.Latency.Degraded(elapsed) {
		return models.StatusDegraded, elapsed,
			fmt.Sprintf("HTTP %d but slow: %s", resp.StatusCode, p.Latency.Describe(elapsed)), nil
	}
	return models.StatusHealthy, elapsed, fmt.Sprintf("HTTP %d", resp.StatusCode), nil
}

// ----------------------------------------------------------------- TCP

// TCPProbe checks that a TCP port accepts a connection.
type TCPProbe struct {
	Address string // host:port
}

// Run dials the address.
func (p *TCPProbe) Run(ctx context.Context) (models.Status, time.Duration, string, error) {
	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", p.Address)
	elapsed := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			// Timed out rather than actively refused: inconclusive.
			return models.StatusUnknown, elapsed, "dial timeout", nil
		}
		return models.StatusCritical, elapsed, err.Error(), nil
	}
	_ = conn.Close()
	return models.StatusHealthy, elapsed, "connected", nil
}

// ---------------------------------------------------------------- gRPC

// GRPCProbe checks a gRPC health endpoint over its HTTP/2 transport.
//
// It uses the standard grpc.health.v1.Health/Check path. A non-200 or a
// transport failure is treated the same way as the HTTP probe.
type GRPCProbe struct {
	Address string
	Service string
	Client  *http.Client
	// Latency optionally downgrades a slow-but-serving response, on the same
	// terms as the HTTP probe.
	Latency LatencyPolicy
}

// Run performs the health RPC.
func (p *GRPCProbe) Run(ctx context.Context) (models.Status, time.Duration, string, error) {
	url := fmt.Sprintf("%s/grpc.health.v1.Health/Check", strings.TrimSuffix(p.Address, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(""))
	if err != nil {
		return models.StatusUnknown, 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")

	start := time.Now()
	resp, err := p.Client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return models.StatusUnknown, elapsed, err.Error(), nil
	}
	defer func() { _ = resp.Body.Close() }()

	// grpc-status 0 in the trailer (or header, for trailers-only responses)
	// means SERVING.
	if code := resp.Header.Get("grpc-status"); code != "" && code != "0" {
		return models.StatusCritical, elapsed, "grpc-status " + code, nil
	}
	if resp.StatusCode != http.StatusOK {
		return models.StatusCritical, elapsed, fmt.Sprintf("HTTP %d", resp.StatusCode), nil
	}
	if p.Latency.Degraded(elapsed) {
		return models.StatusDegraded, elapsed, "SERVING but slow: " + p.Latency.Describe(elapsed), nil
	}
	return models.StatusHealthy, elapsed, "SERVING", nil
}

// -------------------------------------------------------------- custom

// CustomProbe shells out to an operator-supplied command.
//
// The command must be on the allow-list; exit code 0 is healthy, 1 is
// degraded, anything else is critical. A command that cannot be started at all
// is unknown.
type CustomProbe struct {
	Path      string
	Args      []string
	Allowlist func(string) bool
}

// Run executes the command.
func (p *CustomProbe) Run(ctx context.Context) (models.Status, time.Duration, string, error) {
	if p.Allowlist == nil || !p.Allowlist(p.Path) {
		return models.StatusUnknown, 0, "", fmt.Errorf("custom probe binary %q is not on the allow-list", p.Path)
	}

	start := time.Now()
	out, err := exec.CommandContext(ctx, p.Path, p.Args...).CombinedOutput()
	elapsed := time.Since(start)
	detail := strings.TrimSpace(string(out))
	if len(detail) > 500 {
		detail = detail[:500]
	}

	if err == nil {
		return models.StatusHealthy, elapsed, detail, nil
	}
	var ee *exec.ExitError
	if !asExitError(err, &ee) {
		return models.StatusUnknown, elapsed, detail, fmt.Errorf("run custom probe: %w", err)
	}
	if ee.ExitCode() == 1 {
		return models.StatusDegraded, elapsed, detail, nil
	}
	return models.StatusCritical, elapsed, detail, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// ----------------------------------------------------------- TLS client

// NewHTTPClient builds an HTTP client for probing.
//
// If caCertSecretEnv is set, the named environment variable must contain a PEM
// bundle; the client then trusts only that bundle. Resolution failure is an
// error so the check fails closed rather than silently probing with the system
// trust store.
func NewHTTPClient(timeout time.Duration, caCertSecretEnv string) (*http.Client, error) {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout: timeout,
		MaxIdleConnsPerHost: 4,
		// Probes are short-lived and frequent; do not hold connections open
		// long enough to mask a restarted backend.
		IdleConnTimeout: 30 * time.Second,
	}

	if caCertSecretEnv != "" {
		pem, err := config.Resolve(caCertSecretEnv)
		if err != nil {
			return nil, fmt.Errorf("resolve CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("CA bundle in %s contains no valid certificates", caCertSecretEnv)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	return &http.Client{Timeout: timeout, Transport: transport}, nil
}
