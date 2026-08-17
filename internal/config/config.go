// Package config loads runtime configuration from the environment.
//
// Secrets are never read from config files. Every credential is referenced by
// the *name* of an environment variable, and resolved with Resolve at call
// time, so the value only ever lives in process memory.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved server configuration.
type Config struct {
	Addr        string
	DatabaseURL string

	// Probing defaults, overridable per service in Admin.
	ProbeInterval    time.Duration
	ProbeTimeout     time.Duration
	FailuresToTrip   int
	SuccessToRecover int

	// Fail-open policy.
	QuorumRequired int
	QuorumTotal    int
	FailOpenHold   time.Duration

	// Auth.
	LDAPURL         string
	LDAPBindDN      string
	LDAPBindPassEnv string
	LDAPBaseDN      string
	LDAPUserFilter  string
	LDAPGroupFilter string
	LDAPAdminGroup  string
	LDAPCacheTTL    time.Duration
	AuthDisabled    bool
	DevUser         string
	DevGroups       []string

	// Envoy control plane for LB weight pushes.
	EnvoyXDSAddr string

	// Allow-list of binaries the custom SSO credential_process may execute.
	SSOBinaryAllowlist []string

	CORSOrigins []string
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:               env("HTTP_ADDR", ":8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		ProbeInterval:      envDuration("PROBE_INTERVAL", 5*time.Second),
		ProbeTimeout:       envDuration("PROBE_TIMEOUT", 3*time.Second),
		FailuresToTrip:     envInt("FAILURES_TO_TRIP", 3),
		SuccessToRecover:   envInt("SUCCESS_TO_RECOVER", 2),
		QuorumRequired:     envInt("QUORUM_REQUIRED", 2),
		QuorumTotal:        envInt("QUORUM_TOTAL", 3),
		FailOpenHold:       envDuration("FAIL_OPEN_HOLD", 10*time.Minute),
		LDAPURL:            os.Getenv("LDAP_URL"),
		LDAPBindDN:         os.Getenv("LDAP_BIND_DN"),
		LDAPBindPassEnv:    env("LDAP_BIND_PASSWORD_ENV", "LDAP_BIND_PASSWORD"),
		LDAPBaseDN:         os.Getenv("LDAP_BASE_DN"),
		LDAPUserFilter:     env("LDAP_USER_FILTER", "(uid=%s)"),
		LDAPGroupFilter:    env("LDAP_GROUP_FILTER", "(member=%s)"),
		LDAPAdminGroup:     env("LDAP_ADMIN_GROUP", "sre-admins"),
		LDAPCacheTTL:       envDuration("LDAP_CACHE_TTL", 5*time.Minute),
		AuthDisabled:       envBool("AUTH_DISABLED", false),
		DevUser:            env("DEV_USER", "dev@localhost"),
		DevGroups:          envList("DEV_GROUPS", []string{"sre-admins"}),
		EnvoyXDSAddr:       os.Getenv("ENVOY_XDS_ADDR"),
		SSOBinaryAllowlist: envList("SSO_BINARY_ALLOWLIST", nil),
		CORSOrigins:        envList("CORS_ORIGINS", []string{"http://localhost:5173"}),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if !c.AuthDisabled && c.LDAPURL == "" {
		return nil, fmt.Errorf("LDAP_URL is required unless AUTH_DISABLED=true")
	}
	if c.QuorumRequired > c.QuorumTotal {
		return nil, fmt.Errorf("QUORUM_REQUIRED (%d) exceeds QUORUM_TOTAL (%d)", c.QuorumRequired, c.QuorumTotal)
	}
	return c, nil
}

// Resolve reads the value of the environment variable named by ref.
//
// This is the single chokepoint for turning a stored reference into a live
// secret. It fails closed: an unset or empty variable is an error, so a
// misconfigured check refuses to run rather than silently probing without
// credentials. The value is never logged.
func Resolve(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("no secret reference configured")
	}
	v := os.Getenv(ref)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("environment variable %q is unset or empty", ref)
	}
	return v, nil
}

// BinaryAllowed reports whether path is on the custom SSO binary allow-list.
func (c *Config) BinaryAllowed(path string) bool {
	for _, p := range c.SSOBinaryAllowlist {
		if p == path {
			return true
		}
	}
	return false
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envList(k string, def []string) []string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
