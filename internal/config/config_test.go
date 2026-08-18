package config

import (
	"testing"
	"time"
)

// setEnv sets the minimum required variables plus any overrides, and restores
// the environment afterwards via t.Setenv.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AUTH_DISABLED", "true")
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestDefaults(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.UnknownsToDistrust != 3 {
		t.Errorf("UnknownsToDistrust = %d, want 3", c.UnknownsToDistrust)
	}
	if c.EvaluateInterval != 15*time.Second {
		t.Errorf("EvaluateInterval = %s, want 15s", c.EvaluateInterval)
	}
	if c.QuorumRequired != 2 || c.QuorumTotal != 3 {
		t.Errorf("quorum = %d-of-%d, want 2-of-3", c.QuorumRequired, c.QuorumTotal)
	}
	if c.FailOpenHold != 10*time.Minute {
		t.Errorf("FailOpenHold = %s, want 10m", c.FailOpenHold)
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"UNKNOWNS_TO_DISTRUST": "5",
		"EVALUATE_INTERVAL":    "45s",
		"PROBE_INTERVAL":       "2s",
		"PROBE_TIMEOUT":        "1500ms",
		"FAILURES_TO_TRIP":     "4",
		"SUCCESS_TO_RECOVER":   "3",
		"QUORUM_REQUIRED":      "3",
		"QUORUM_TOTAL":         "5",
		"FAIL_OPEN_HOLD":       "2m",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"UnknownsToDistrust", c.UnknownsToDistrust, 5},
		{"EvaluateInterval", c.EvaluateInterval, 45 * time.Second},
		{"ProbeInterval", c.ProbeInterval, 2 * time.Second},
		{"ProbeTimeout", c.ProbeTimeout, 1500 * time.Millisecond},
		{"FailuresToTrip", c.FailuresToTrip, 4},
		{"SuccessToRecover", c.SuccessToRecover, 3},
		{"QuorumRequired", c.QuorumRequired, 3},
		{"QuorumTotal", c.QuorumTotal, 5},
		{"FailOpenHold", c.FailOpenHold, 2 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestRejectsInvalidQuorumAndTuning(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"required exceeds total", map[string]string{"QUORUM_REQUIRED": "4", "QUORUM_TOTAL": "3"}},
		// Equal values leave zero unknown tolerance, which would distrust a
		// region on its first inconclusive probe and hold fail-open forever.
		{"required equals total", map[string]string{"QUORUM_REQUIRED": "3", "QUORUM_TOTAL": "3"}},
		{"required below one", map[string]string{"QUORUM_REQUIRED": "0", "QUORUM_TOTAL": "3"}},
		{"unknowns below one", map[string]string{"UNKNOWNS_TO_DISTRUST": "0"}},
		{"evaluate interval zero", map[string]string{"EVALUATE_INTERVAL": "0s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			if _, err := Load(); err == nil {
				t.Fatal("expected a validation error, got none")
			}
		})
	}
}

func TestDatabaseURLRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AUTH_DISABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("expected DATABASE_URL to be required")
	}
}

func TestLDAPRequiredUnlessAuthDisabled(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AUTH_DISABLED", "false")
	t.Setenv("LDAP_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected LDAP_URL to be required when auth is enabled")
	}
}

// TestResolveFailsClosed pins the secret-resolution contract: a missing or
// blank reference is an error, never a silent fallback.
func TestResolveFailsClosed(t *testing.T) {
	if _, err := Resolve(""); err == nil {
		t.Error("empty reference should error")
	}
	t.Setenv("SOME_UNSET_SECRET", "")
	if _, err := Resolve("SOME_UNSET_SECRET"); err == nil {
		t.Error("unset variable should error")
	}
	t.Setenv("SOME_SECRET", "   ")
	if _, err := Resolve("SOME_SECRET"); err == nil {
		t.Error("whitespace-only value should error")
	}
	t.Setenv("SOME_SECRET", "value")
	got, err := Resolve("SOME_SECRET")
	if err != nil || got != "value" {
		t.Errorf("Resolve = %q, %v; want \"value\", nil", got, err)
	}
}
