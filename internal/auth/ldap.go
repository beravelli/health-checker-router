// Package auth authenticates callers against LDAP and resolves their group
// memberships.
//
// Group membership is the source of truth for admin access and is enforced
// server-side. The UI's role switcher is a prototype affordance only; nothing
// here trusts a client-supplied group claim.
//
// Memberships are cached briefly so a directory lookup does not run on every
// request, and the cache is invalidated on logout.
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/models"
)

// Directory resolves users and their groups.
type Directory interface {
	// Authenticate verifies credentials and returns the user with groups.
	Authenticate(ctx context.Context, username, password string) (models.User, error)
	// Groups resolves group membership for an already-authenticated user.
	Groups(ctx context.Context, username string) ([]string, error)
	// Invalidate drops any cached membership for a user.
	Invalidate(username string)
}

type cacheEntry struct {
	groups    []string
	expiresAt time.Time
}

// LDAPDirectory is a Directory backed by an LDAP server.
type LDAPDirectory struct {
	cfg *config.Config
	log *slog.Logger

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// NewLDAPDirectory constructs a directory client.
func NewLDAPDirectory(cfg *config.Config, log *slog.Logger) *LDAPDirectory {
	return &LDAPDirectory{cfg: cfg, log: log, cache: map[string]cacheEntry{}}
}

// dial opens a connection and binds as the service account.
func (d *LDAPDirectory) dial() (*ldap.Conn, error) {
	conn, err := ldap.DialURL(d.cfg.LDAPURL)
	if err != nil {
		return nil, fmt.Errorf("dial LDAP: %w", err)
	}
	conn.SetTimeout(10 * time.Second)

	if d.cfg.LDAPBindDN != "" {
		password, err := config.Resolve(d.cfg.LDAPBindPassEnv)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("resolve LDAP bind password: %w", err)
		}
		if err := conn.Bind(d.cfg.LDAPBindDN, password); err != nil {
			conn.Close()
			return nil, fmt.Errorf("bind service account: %w", err)
		}
	}
	return conn, nil
}

// Authenticate verifies a username and password, then resolves groups.
func (d *LDAPDirectory) Authenticate(ctx context.Context, username, password string) (models.User, error) {
	if username == "" || password == "" {
		return models.User{}, fmt.Errorf("username and password are required")
	}

	if err := d.verifyPassword(username, password); err != nil {
		return models.User{}, err
	}

	// Group membership is resolved on a fresh service-account connection: the
	// user's own bind may not have read access to group objects.
	groups, err := d.Groups(ctx, username)
	if err != nil {
		return models.User{}, err
	}
	return models.User{Username: username, Groups: groups}, nil
}

// verifyPassword binds as the user to check their credentials, on a connection
// that is closed before anything else happens on it.
func (d *LDAPDirectory) verifyPassword(username, password string) error {
	conn, err := d.dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	userDN, err := d.findUserDN(conn, username)
	if err != nil {
		return err
	}
	if err := conn.Bind(userDN, password); err != nil {
		return fmt.Errorf("authentication failed")
	}
	return nil
}

// Groups resolves a user's group memberships, using the cache when warm.
func (d *LDAPDirectory) Groups(_ context.Context, username string) ([]string, error) {
	d.mu.RLock()
	if e, ok := d.cache[username]; ok && time.Now().Before(e.expiresAt) {
		d.mu.RUnlock()
		return e.groups, nil
	}
	d.mu.RUnlock()

	conn, err := d.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	userDN, err := d.findUserDN(conn, username)
	if err != nil {
		return nil, err
	}

	filter := fmt.Sprintf(d.cfg.LDAPGroupFilter, ldap.EscapeFilter(userDN))
	res, err := conn.Search(ldap.NewSearchRequest(
		d.cfg.LDAPBaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 10, false,
		filter,
		[]string{"cn"},
		nil,
	))
	if err != nil {
		return nil, fmt.Errorf("search groups: %w", err)
	}

	groups := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		if cn := e.GetAttributeValue("cn"); cn != "" {
			groups = append(groups, cn)
		}
	}

	d.mu.Lock()
	d.cache[username] = cacheEntry{groups: groups, expiresAt: time.Now().Add(d.cfg.LDAPCacheTTL)}
	d.mu.Unlock()

	return groups, nil
}

// Invalidate clears a user's cached memberships.
func (d *LDAPDirectory) Invalidate(username string) {
	d.mu.Lock()
	delete(d.cache, username)
	d.mu.Unlock()
}

func (d *LDAPDirectory) findUserDN(conn *ldap.Conn, username string) (string, error) {
	filter := fmt.Sprintf(d.cfg.LDAPUserFilter, ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(
		d.cfg.LDAPBaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 10, false,
		filter,
		[]string{"dn"},
		nil,
	))
	if err != nil {
		return "", fmt.Errorf("search user: %w", err)
	}
	if len(res.Entries) == 0 {
		return "", fmt.Errorf("authentication failed")
	}
	if len(res.Entries) > 1 {
		return "", fmt.Errorf("user filter matched %d entries; refine LDAP_USER_FILTER", len(res.Entries))
	}
	return res.Entries[0].DN, nil
}

// StaticDirectory is a Directory for local development, enabled by
// AUTH_DISABLED=true. It never talks to a network.
type StaticDirectory struct {
	User      string
	GroupList []string
}

// Authenticate accepts any password and returns the configured dev user.
func (s StaticDirectory) Authenticate(_ context.Context, username, _ string) (models.User, error) {
	if username == "" {
		username = s.User
	}
	return models.User{Username: username, Groups: s.GroupList}, nil
}

// Groups returns the configured dev groups.
func (s StaticDirectory) Groups(_ context.Context, _ string) ([]string, error) {
	return s.GroupList, nil
}

// Invalidate is a no-op.
func (s StaticDirectory) Invalidate(string) {}

// NormalizeGroup lowercases and trims a group name for comparison.
func NormalizeGroup(g string) string { return strings.ToLower(strings.TrimSpace(g)) }
