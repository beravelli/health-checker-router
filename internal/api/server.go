// Package api exposes the REST and SSE surface the dashboard consumes.
//
// Routing uses the standard library's method-aware ServeMux (Go 1.22+), so
// there is no third-party router in the dependency tree.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/beravelli/health-checker-router/internal/auth"
	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/failopen"
	"github.com/beravelli/health-checker-router/internal/health"
	"github.com/beravelli/health-checker-router/internal/incident"
	"github.com/beravelli/health-checker-router/internal/lb"
	"github.com/beravelli/health-checker-router/internal/rollout"
	"github.com/beravelli/health-checker-router/internal/routing"
	"github.com/beravelli/health-checker-router/internal/store"
)

// ServiceSyncer is told when the registered service list changes, so newly
// added probe targets start without a restart.
type ServiceSyncer interface {
	SyncNow(ctx context.Context) error
}

// Server wires the HTTP surface to the domain services.
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	store     *store.Store
	agg       *health.Aggregator
	lb        *lb.Controller
	failopen  *failopen.Controller
	incidents *incident.Service
	rollouts  *rollout.Service
	registry  *routing.Registry
	hub       *events.Hub
	dir       auth.Directory
	sessions  *auth.SessionStore
	syncer    ServiceSyncer
}

// Deps groups the Server's collaborators.
type Deps struct {
	Config     *config.Config
	Log        *slog.Logger
	Store      *store.Store
	Aggregator *health.Aggregator
	LB         *lb.Controller
	FailOpen   *failopen.Controller
	Incidents  *incident.Service
	Rollouts   *rollout.Service
	Registry   *routing.Registry
	Hub        *events.Hub
	Directory  auth.Directory
	Sessions   *auth.SessionStore
	Syncer     ServiceSyncer
}

// New constructs a Server.
func New(d Deps) *Server {
	return &Server{
		cfg: d.Config, log: d.Log, store: d.Store, agg: d.Aggregator,
		lb: d.LB, failopen: d.FailOpen, incidents: d.Incidents,
		rollouts: d.Rollouts, registry: d.Registry, hub: d.Hub,
		dir: d.Directory, sessions: d.Sessions, syncer: d.Syncer,
	}
}

// Handler builds the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness and readiness are unauthenticated so kubelet can reach them.
	mux.HandleFunc("GET /healthz", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)

	// Session.
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)

	// Read surface — any authenticated user.
	mux.HandleFunc("GET /api/regions", requireUser(s.handleRegions))
	mux.HandleFunc("GET /api/services", requireUser(s.handleServices))
	mux.HandleFunc("GET /api/health", requireUser(s.handleHealth))
	mux.HandleFunc("GET /api/incidents", requireUser(s.handleIncidents))
	mux.HandleFunc("GET /api/rollouts", requireUser(s.handleRollouts))
	mux.HandleFunc("GET /api/lb/weights", requireUser(s.handleGetWeights))
	mux.HandleFunc("GET /api/failopen", requireUser(s.handleFailOpen))
	mux.HandleFunc("GET /api/routing/endpoints", requireUser(s.handleRoutingEndpoints))
	mux.HandleFunc("GET /api/events", requireUser(s.handleEvents))

	// Mutations — admin group only. Traffic-moving actions are gated the same
	// way as configuration changes, since both can take a region out of
	// service.
	mux.HandleFunc("POST /api/lb/weights", s.requireAdmin(s.handleSetWeights))
	mux.HandleFunc("POST /api/lb/reset", s.requireAdmin(s.handleResetWeights))
	mux.HandleFunc("POST /api/lb/failover", s.requireAdmin(s.handleFailover))

	mux.HandleFunc("POST /api/rollouts/{id}/pause", s.requireAdmin(s.handleRolloutPause))
	mux.HandleFunc("POST /api/rollouts/{id}/resume", s.requireAdmin(s.handleRolloutResume))
	mux.HandleFunc("POST /api/rollouts/{id}/rollback", s.requireAdmin(s.handleRolloutRollback))

	mux.HandleFunc("POST /api/admin/services", s.requireAdmin(s.handleCreateService))
	mux.HandleFunc("PATCH /api/admin/services/{id}", s.requireAdmin(s.handleUpdateServiceRole))
	mux.HandleFunc("DELETE /api/admin/services/{id}", s.requireAdmin(s.handleDeleteService))

	mux.HandleFunc("POST /api/admin/regions", s.requireAdmin(s.handleUpsertRegion))
	mux.HandleFunc("DELETE /api/admin/regions/{id}", s.requireAdmin(s.handleDeleteRegion))

	mux.HandleFunc("PUT /api/admin/routing/{provider}", s.requireAdmin(s.handleUpsertRouting))
	mux.HandleFunc("GET /api/admin/audit", s.requireAdmin(s.handleAudit))

	// The console bundle, on everything the API did not claim. Registered last
	// and rooted at "/" so it is the lowest-precedence pattern in the mux.
	if s.cfg.WebDir != "" {
		mux.Handle("/", staticHandler(s.cfg.WebDir))
	}

	// Order matters: recover outermost, then logging, CORS, and finally auth.
	return s.recoverPanic(s.requestLogger(s.cors(s.authenticate(mux))))
}

// Serve runs the HTTP server until ctx is cancelled, then drains.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.cfg.Addr,
		Handler: s.Handler(),
		// No WriteTimeout: SSE connections are long-lived by design and a
		// write deadline would sever them mid-stream.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s.log.Info("shutting down")
		return srv.Shutdown(shutdownCtx)
	}
}
