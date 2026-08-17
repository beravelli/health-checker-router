// Package models holds the domain types shared across the health checker.
//
// Status values are typed enums rather than bare strings so that the tri-state
// semantics the UI depends on (healthy / degraded / critical, plus unknown for
// "we genuinely do not know") cannot be silently widened by a typo.
package models

import (
	"fmt"
	"time"
)

// Status is the health of a probe target, service, or region.
type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusDegraded Status = "degraded"
	StatusCritical Status = "critical"
	// StatusUnknown means the probe could not reach a verdict. It is distinct
	// from StatusCritical on purpose: unknown drives fail-open, critical drives
	// failover.
	StatusUnknown Status = "unknown"
)

// Valid reports whether s is a recognised status.
func (s Status) Valid() bool {
	switch s {
	case StatusHealthy, StatusDegraded, StatusCritical, StatusUnknown:
		return true
	}
	return false
}

// Score maps a status onto a 0..1 weight contribution used by the aggregator.
func (s Status) Score() float64 {
	switch s {
	case StatusHealthy:
		return 1.0
	case StatusDegraded:
		return 0.5
	case StatusCritical:
		return 0.0
	default:
		return 1.0 // unknown does not drag the score down; fail-open handles it
	}
}

// ParseStatus converts a string to a Status, rejecting unknown values.
func ParseStatus(v string) (Status, error) {
	s := Status(v)
	if !s.Valid() {
		return "", fmt.Errorf("invalid status %q", v)
	}
	return s, nil
}

// ProbeType is how a target is checked.
type ProbeType string

const (
	ProbeHTTP   ProbeType = "http"
	ProbeTCP    ProbeType = "tcp"
	ProbeALB    ProbeType = "aws_alb"
	ProbeNLB    ProbeType = "aws_nlb"
	ProbeGRPC   ProbeType = "grpc"
	ProbeCustom ProbeType = "custom"
)

// ProbeRole controls whether a service's health may move traffic.
type ProbeRole string

const (
	// RoleHealthcheckOnly services are observed but never influence LB weight.
	RoleHealthcheckOnly ProbeRole = "healthcheck_only"
	// RoleHealthcheckFailover services feed the composite region score.
	RoleHealthcheckFailover ProbeRole = "healthcheck_failover"
)

// RoutingProvider identifies the external traffic manager fronting the app.
type RoutingProvider string

const (
	ProviderAWSNative      RoutingProvider = "aws_native"
	ProviderCloudFront     RoutingProvider = "cloudfront"
	ProviderAviGSLB        RoutingProvider = "avi_gslb"
	ProviderAkamaiCloudlet RoutingProvider = "akamai_cloudlet"
)

// AWSAuthMethod is how the prober obtains AWS credentials for a target.
type AWSAuthMethod string

const (
	AuthAssumeRole      AWSAuthMethod = "assume_role"
	AuthServiceAccount  AWSAuthMethod = "service_account"
	AuthAWSSSO          AWSAuthMethod = "aws_sso"
	AuthCustomSSOBinary AWSAuthMethod = "custom_sso_binary"
)

// Severity classifies an incident.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityResolved Severity = "resolved"
)

// Region is one active-active site.
type Region struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	Provider    string    `json:"provider"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Service is a registered probe target.
type Service struct {
	ID                          string        `json:"id"`
	Name                        string        `json:"name"`
	Runtime                     string        `json:"runtime"`
	Role                        ProbeRole     `json:"role"`
	ProbeType                   ProbeType     `json:"probeType"`
	ProbeIntervalS              int           `json:"probeIntervalS"`
	ProbeTimeoutS               int           `json:"probeTimeoutS"`
	ConsecutiveFailuresToTrip   int           `json:"consecutiveFailuresToTrip"`
	ConsecutiveSuccessToRecover int           `json:"consecutiveSuccessToRecover"`
	CACertSecretEnv             string        `json:"caCertSecretEnv,omitempty"`
	AWSAuthMethod               AWSAuthMethod `json:"awsAuthMethod,omitempty"`
	AWSAuthRef                  string        `json:"awsAuthRef,omitempty"`
	// ProbeConfig holds probe-type-specific settings (path, port, expected
	// status, target group ARN, gRPC service name, custom command...).
	ProbeConfig map[string]any  `json:"probeConfig"`
	Regions     []ServiceRegion `json:"regions"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

// ServiceRegion is a service's presence in one region.
type ServiceRegion struct {
	RegionID string `json:"regionId"`
	Version  string `json:"version,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// ProbeResult is one probe observation.
type ProbeResult struct {
	ServiceID string    `json:"serviceId"`
	RegionID  string    `json:"regionId"`
	Status    Status    `json:"status"`
	LatencyMs int       `json:"latencyMs"`
	Detail    string    `json:"detail,omitempty"`
	ProbedAt  time.Time `json:"probedAt"`
}

// DependencyStatus is the health of a region-level dependency (Postgres,
// Redis, Kafka, Envoy edge).
type DependencyStatus struct {
	RegionID  string    `json:"regionId"`
	Name      string    `json:"name"`
	Status    Status    `json:"status"`
	CheckedAt time.Time `json:"checkedAt"`
}

// RegionHealth is the composite view the Overview tab renders.
type RegionHealth struct {
	RegionID         string             `json:"regionId"`
	Name             string             `json:"name"`
	Zone             string             `json:"zone"`
	Status           Status             `json:"status"`
	Score            float64            `json:"score"`
	TrafficPct       int                `json:"trafficPct"`
	LatencyMs        int                `json:"latencyMs"`
	ErrorRatePct     float64            `json:"errorRatePct"`
	ReplicationLagMs int                `json:"replicationLagMs"`
	UptimePct30d     float64            `json:"uptimePct"`
	Version          string             `json:"version"`
	AZHealthy        string             `json:"azHealthy"`
	Nodes            int                `json:"nodes"`
	Proxies          int                `json:"proxies"`
	LastProbe        time.Time          `json:"lastProbe"`
	Dependencies     []DependencyStatus `json:"deps"`
	Trend            []TrendPoint       `json:"trend"`
}

// TrendPoint is one day of the 7-day uptime trend.
type TrendPoint struct {
	Day       string  `json:"day"`
	UptimePct float64 `json:"uptimePct"`
}

// ServiceHealth is one row of the service matrix.
type ServiceHealth struct {
	ServiceID string            `json:"serviceId"`
	Name      string            `json:"name"`
	Runtime   string            `json:"runtime"`
	ByRegion  map[string]Status `json:"byRegion"`
	Versions  map[string]string `json:"versions"`
	Skew      bool              `json:"skew"`
	Note      string            `json:"note"`
}

// LBWeight is the current traffic weight for a region.
type LBWeight struct {
	RegionID    string    `json:"regionId"`
	Weight      int       `json:"weight"`
	IsAutomatic bool      `json:"isAutomatic"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// FailOpenEvent records a window where health was inconclusive and the LB held
// last-known-good weights.
type FailOpenEvent struct {
	ID          int64      `json:"id"`
	TriggeredAt time.Time  `json:"triggeredAt"`
	ResolvedAt  *time.Time `json:"resolvedAt,omitempty"`
	Reason      string     `json:"reason"`
}

// Active reports whether the fail-open window is still open.
func (f FailOpenEvent) Active() bool { return f.ResolvedAt == nil }

// Rollout tracks a per-service deploy across regions.
type Rollout struct {
	ID          string    `json:"id"`
	ServiceID   string    `json:"serviceId"`
	Service     string    `json:"service"`
	FromVersion string    `json:"fromVersion"`
	ToVersion   string    `json:"toVersion"`
	Status      string    `json:"status"`
	EastPct     int       `json:"eastPct"`
	WestPct     int       `json:"westPct"`
	Paused      bool      `json:"paused"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Incident is a timeline entry.
type Incident struct {
	ID          string    `json:"id"`
	OccurredAt  time.Time `json:"occurredAt"`
	Severity    Severity  `json:"severity"`
	RegionID    string    `json:"regionId"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
}

// RoutingEndpoint is one configured traffic-manager provider.
//
// Auth is stored as a *reference* (env var name, secret path, SSO profile),
// never as a credential value.
type RoutingEndpoint struct {
	Provider   RoutingProvider `json:"provider"`
	Enabled    bool            `json:"enabled"`
	Config     map[string]any  `json:"config"`
	AuthMethod string          `json:"authMethod,omitempty"`
	AuthRef    string          `json:"authRef,omitempty"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// AuditEntry records an admin mutation.
type AuditEntry struct {
	ID     int64     `json:"id"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	At     time.Time `json:"at"`
}

// User is the authenticated caller and their resolved LDAP groups.
type User struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
}

// InGroup reports whether the user belongs to the named group.
func (u User) InGroup(name string) bool {
	for _, g := range u.Groups {
		if g == name {
			return true
		}
	}
	return false
}
