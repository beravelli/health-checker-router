package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/beravelli/health-checker-router/internal/auth"
	"github.com/beravelli/health-checker-router/internal/models"
)

// errorBody is the shape of every error response.
//
// RequiredGroup is populated on 403 so the frontend can render the same
// "you need group X" message the Admin tab shows, without hardcoding the
// group name client-side.
type errorBody struct {
	Error         string `json:"error"`
	RequiredGroup string `json:"requiredGroup,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; all that is left is to record it.
		slog.Error("encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// decodeJSON reads a JSON body with a size cap, rejecting unknown fields so a
// typo'd field name fails loudly instead of being silently ignored.
func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// requestLogger logs each request with its status and duration.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// SSE streams run for minutes; logging them on completion is noise.
		if strings.HasSuffix(r.URL.Path, "/events") {
			return
		}
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE still streams through the
// wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// cors applies a strict allow-list. Credentials are permitted because the
// session travels in a cookie, which rules out a wildcard origin.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the session cookie into a user on the request context.
// It does not reject unauthenticated requests; requireUser does that.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthDisabled {
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), models.User{
				Username: s.cfg.DevUser,
				Groups:   s.cfg.DevGroups,
			})))
			return
		}

		token := auth.TokenFrom(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := s.sessions.Get(token)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), sess.User)))
	})
}

// requireUser rejects unauthenticated requests.
func requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserFrom(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

// requireAdmin gates a handler behind the configured admin LDAP group.
//
// This is the real access control. The frontend's role switcher is a display
// affordance; every mutating admin route passes through here, and the 403 names
// the group so the UI can render the access-restricted card accurately.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !user.InGroup(s.cfg.LDAPAdminGroup) {
			s.log.Warn("admin action denied",
				"user", user.Username, "path", r.URL.Path, "required", s.cfg.LDAPAdminGroup)
			writeJSON(w, http.StatusForbidden, errorBody{
				Error:         "admin access requires LDAP group membership",
				RequiredGroup: s.cfg.LDAPAdminGroup,
			})
			return
		}
		next(w, r)
	}
}

// recoverPanic keeps one bad handler from taking the process down.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
