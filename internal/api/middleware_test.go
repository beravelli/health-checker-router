package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beravelli/health-checker-router/internal/auth"
	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
)

// These tests cover the access control that stands between an authenticated
// user and moving production traffic. The frontend's role switcher is a display
// affordance; requireAdmin is the actual gate, and every mutating route —
// including the traffic-moving ones — passes through it.

func testServer(cfg *config.Config) *Server {
	if cfg.LDAPAdminGroup == "" {
		cfg.LDAPAdminGroup = "sre-admins"
	}
	return &Server{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: auth.NewSessionStore(time.Hour),
	}
}

// reached reports whether the wrapped handler ran, which is the only thing that
// matters: a gate that returns 403 but still executes the handler is no gate.
func gated(s *Server, admin bool) (http.HandlerFunc, *bool) {
	ran := false
	h := func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}
	if admin {
		return s.requireAdmin(h), &ran
	}
	return requireUser(h), &ran
}

func withUser(r *http.Request, username string, groups ...string) *http.Request {
	return r.WithContext(auth.WithUser(r.Context(),
		models.User{Username: username, Groups: groups}))
}

func TestRequireAdminAllowsGroupMember(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})
	h, ran := gated(s, true)

	rec := httptest.NewRecorder()
	h(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/lb/failover", nil),
		"rohith", "developers", "sre-admins"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !*ran {
		t.Error("handler did not run for an admin-group member")
	}
}

func TestRequireAdminDeniesNonMember(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})
	h, ran := gated(s, true)

	rec := httptest.NewRecorder()
	h(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/lb/failover", nil),
		"intern", "developers", "sre-readonly"))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if *ran {
		t.Fatal("handler ran despite the 403 — the gate does not gate")
	}

	// The 403 names the required group so the UI can render it without
	// hardcoding the group name client-side.
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.RequiredGroup != "sre-admins" {
		t.Errorf("requiredGroup = %q, want sre-admins", body.RequiredGroup)
	}
}

func TestRequireAdminDeniesUnauthenticated(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})
	h, ran := gated(s, true)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/lb/failover", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if *ran {
		t.Fatal("handler ran for an unauthenticated request")
	}
}

// TestRequireAdminGroupMatchIsExact guards against a substring or
// case-insensitive match letting the wrong group through.
func TestRequireAdminGroupMatchIsExact(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})

	nearMisses := []string{
		"sre-admin",       // singular
		"sre-admins-read", // longer
		"SRE-Admins",      // different case
		"admins",          // suffix only
		" sre-admins",     // leading space
		"sre-admins ",     // trailing space
	}
	for _, group := range nearMisses {
		h, ran := gated(s, true)
		rec := httptest.NewRecorder()
		h(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/admin/regions", nil), "u", group))

		if rec.Code != http.StatusForbidden {
			t.Errorf("group %q got status %d, want 403", group, rec.Code)
		}
		if *ran {
			t.Errorf("group %q reached the handler", group)
		}
	}
}

func TestRequireUserRejectsAnonymous(t *testing.T) {
	s := testServer(&config.Config{})
	h, ran := gated(s, false)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if *ran {
		t.Error("handler ran for an anonymous request")
	}
}

// TestRequireUserDoesNotRequireAdmin: read routes are open to any authenticated
// user, admin group or not.
func TestRequireUserDoesNotRequireAdmin(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})
	h, ran := gated(s, false)

	rec := httptest.NewRecorder()
	h(rec, withUser(httptest.NewRequest(http.MethodGet, "/api/health", nil), "viewer", "developers"))

	if rec.Code != http.StatusOK || !*ran {
		t.Errorf("status = %d ran = %v; a non-admin should be able to read", rec.Code, *ran)
	}
}

// ------------------------------------------------------------ authenticate

func TestAuthenticateResolvesValidSession(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})
	token, _, err := s.sessions.Create(models.User{Username: "rohith", Groups: []string{"sre-admins"}})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	var seen models.User
	var ok bool
	h := s.authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, ok = auth.UserFrom(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.AddCookie(&http.Cookie{Name: "hcr_session", Value: token})
	h.ServeHTTP(httptest.NewRecorder(), r)

	if !ok {
		t.Fatal("valid session did not resolve to a user")
	}
	if seen.Username != "rohith" || !seen.InGroup("sre-admins") {
		t.Errorf("resolved user = %+v", seen)
	}
}

// TestAuthenticateIgnoresBogusToken: an unknown or forged cookie must leave the
// request anonymous rather than erroring, so requireUser/requireAdmin decide.
func TestAuthenticateIgnoresBogusToken(t *testing.T) {
	s := testServer(&config.Config{LDAPAdminGroup: "sre-admins"})

	for _, token := range []string{"not-a-real-token", "", strings.Repeat("a", 64)} {
		var ok bool
		h := s.authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, ok = auth.UserFrom(r.Context())
		}))
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		r.AddCookie(&http.Cookie{Name: "hcr_session", Value: token})
		h.ServeHTTP(httptest.NewRecorder(), r)

		if ok {
			t.Errorf("token %q resolved to a user", token)
		}
	}
}

// TestAuthDisabledGrantsConfiguredGroups documents the local-development
// bypass, and pins that it is driven entirely by config rather than by anything
// a request can influence.
func TestAuthDisabledGrantsConfiguredGroups(t *testing.T) {
	s := testServer(&config.Config{
		AuthDisabled:   true,
		DevUser:        "dev@localhost",
		DevGroups:      []string{"sre-admins"},
		LDAPAdminGroup: "sre-admins",
	})

	h, ran := gated(s, true)
	wrapped := s.authenticate(http.HandlerFunc(h))

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lb/failover", nil))

	if rec.Code != http.StatusOK || !*ran {
		t.Errorf("auth-disabled request status = %d ran = %v; want admin access", rec.Code, *ran)
	}
}

// TestAuthDisabledStillHonoursGroupConfig: disabling auth does not
// automatically grant admin. If DEV_GROUPS omits the admin group, the gate
// still refuses — the bypass supplies an identity, it does not skip the check.
func TestAuthDisabledStillHonoursGroupConfig(t *testing.T) {
	s := testServer(&config.Config{
		AuthDisabled:   true,
		DevUser:        "dev@localhost",
		DevGroups:      []string{"developers"},
		LDAPAdminGroup: "sre-admins",
	})

	h, ran := gated(s, true)
	wrapped := s.authenticate(http.HandlerFunc(h))

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lb/failover", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if *ran {
		t.Error("handler ran despite DEV_GROUPS lacking the admin group")
	}
}

// ------------------------------------------------------------------- cors

func TestCORSOnlyEchoesAllowedOrigins(t *testing.T) {
	s := testServer(&config.Config{CORSOrigins: []string{"http://localhost:5173"}})
	h := s.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("allowed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		r.Header.Set("Origin", "http://localhost:5173")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
			t.Errorf("allow-origin = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("allow-credentials = %q, want true", got)
		}
	})

	t.Run("denied", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		r.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		// No wildcard, no echo. Credentials travel in a cookie, so echoing an
		// arbitrary origin would be a cross-site request forgery primitive.
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("allow-origin = %q for a disallowed origin; want empty", got)
		}
	})
}

func TestCORSNeverWildcards(t *testing.T) {
	s := testServer(&config.Config{CORSOrigins: []string{"*"}})
	h := s.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	// "*" is matched literally against the Origin header, so it grants nothing.
	// Worth pinning: a wildcard alongside allow-credentials would be rejected
	// by browsers anyway, and silently granting it here would be worse.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a literal \"*\" origin entry granted %q", got)
	}
}

// ---------------------------------------------------------------- recover

func TestRecoverPanicReturns500(t *testing.T) {
	s := testServer(&config.Config{})
	h := s.recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	// The panic value must not reach the client.
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic value leaked into the response body")
	}
}

// ----------------------------------------------------------------- decode

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	var dst struct {
		Name string `json:"name"`
	}
	r := httptest.NewRequest(http.MethodPost, "/api/admin/services",
		strings.NewReader(`{"name":"svc","role":"failover_eligible"}`))

	// A typo'd or unexpected field must fail loudly. Silently ignoring "role"
	// on a service-creation payload would mean a caller believing it granted
	// failover eligibility when it did not.
	if err := decodeJSON(r, &dst); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}
