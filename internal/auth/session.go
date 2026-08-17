package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/beravelli/health-checker-router/internal/models"
)

// sessionCookie is the cookie name holding the opaque session token.
const sessionCookie = "hcr_session"

// contextKey is unexported so no other package can inject a user.
type contextKey struct{}

var userKey contextKey

// Session is a signed-in user.
type Session struct {
	User      models.User
	ExpiresAt time.Time
}

// SessionStore holds active sessions in memory.
//
// In-memory is a deliberate choice for a two-replica dashboard: sessions are
// cheap to re-establish and keeping them out of Postgres avoids a write on
// every login. If you scale beyond a couple of replicas behind a
// non-sticky load balancer, back this with Redis.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	ttl      time.Duration
}

// NewSessionStore returns a store with the given session lifetime.
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	s := &SessionStore{sessions: map[string]Session{}, ttl: ttl}
	go s.reap()
	return s
}

// Create issues a session token for a user.
func (s *SessionStore) Create(u models.User) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	expires := time.Now().Add(s.ttl)

	s.mu.Lock()
	s.sessions[token] = Session{User: u, ExpiresAt: expires}
	s.mu.Unlock()

	return token, expires, nil
}

// Get resolves a token to a session.
func (s *SessionStore) Get(token string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok || time.Now().After(sess.ExpiresAt) {
		return Session{}, false
	}
	return sess, true
}

// Delete removes a session.
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// reap periodically drops expired sessions.
func (s *SessionStore) reap() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for token, sess := range s.sessions {
			if now.After(sess.ExpiresAt) {
				delete(s.sessions, token)
			}
		}
		s.mu.Unlock()
	}
}

// SetCookie writes the session cookie.
//
// Secure is set whenever the request arrived over TLS; behind an ingress that
// terminates TLS, the X-Forwarded-Proto header carries that fact.
func SetCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie.
func ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// TokenFrom reads the session token from a request.
func TokenFrom(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// WithUser attaches a user to a request context.
func WithUser(ctx context.Context, u models.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// UserFrom reads the authenticated user from a context.
func UserFrom(ctx context.Context) (models.User, bool) {
	u, ok := ctx.Value(userKey).(models.User)
	return u, ok
}
