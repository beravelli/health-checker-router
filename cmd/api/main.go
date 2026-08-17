// Command api runs the health checker: the probe fleet, the aggregation and
// fail-open pipeline, the LB weight controller, and the HTTP/SSE API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/beravelli/health-checker-router/internal/api"
	"github.com/beravelli/health-checker-router/internal/auth"
	"github.com/beravelli/health-checker-router/internal/awsauth"
	"github.com/beravelli/health-checker-router/internal/config"
	"github.com/beravelli/health-checker-router/internal/events"
	"github.com/beravelli/health-checker-router/internal/failopen"
	"github.com/beravelli/health-checker-router/internal/health"
	"github.com/beravelli/health-checker-router/internal/incident"
	"github.com/beravelli/health-checker-router/internal/lb"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/prober"
	"github.com/beravelli/health-checker-router/internal/rollout"
	"github.com/beravelli/health-checker-router/internal/routing"
	"github.com/beravelli/health-checker-router/internal/routing/akamai"
	"github.com/beravelli/health-checker-router/internal/routing/avigslb"
	"github.com/beravelli/health-checker-router/internal/routing/awsnative"
	"github.com/beravelli/health-checker-router/internal/routing/cloudfrontadapter"
	"github.com/beravelli/health-checker-router/internal/store"
)

// evaluateInterval is how often health is recomputed and weights reconciled.
// It is deliberately slower than the probe interval: probes gather evidence,
// this loop acts on it, and acting faster than evidence accumulates just adds
// churn.
const evaluateInterval = 15 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Cancelled on SIGINT/SIGTERM so k8s gets a clean drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("connected to database")

	hub := events.NewHub()
	incidents := incident.New(db, hub, log, incident.LogNotifier{Log: log})

	// Routing adapters. All four are registered; only the one enabled in
	// Admin is ever built, and building resolves its credentials.
	awsResolver := awsauth.NewResolver(cfg)
	registry := routing.NewRegistry()
	registry.Register(models.ProviderAWSNative, awsnative.Factory(awsResolver))
	registry.Register(models.ProviderCloudFront, cloudfrontadapter.Factory(awsResolver))
	registry.Register(models.ProviderAviGSLB, avigslb.Factory())
	registry.Register(models.ProviderAkamaiCloudlet, akamai.Factory())

	lbController := lb.New(db, registry, hub, log)

	failOpen := failopen.New(failopen.Policy{
		QuorumRequired: cfg.QuorumRequired,
		QuorumTotal:    cfg.QuorumTotal,
		Hold:           cfg.FailOpenHold,
	}, db, incidents, hub, log)

	probeRunner := prober.NewRunner(cfg, db, &statusLogger{log: log},
		awsauth.NewTargetHealth(awsResolver), log)
	defer probeRunner.Stop()

	aggregator := health.New(db, probeRunner)
	rollouts := rollout.New(db, probeRunner, hub, log)

	syncer := &serviceSyncer{store: db, runner: probeRunner, ctx: ctx}
	if err := syncer.SyncNow(ctx); err != nil {
		return err
	}

	directory := buildDirectory(cfg, log)
	sessions := auth.NewSessionStore(12 * time.Hour)

	server := api.New(api.Deps{
		Config: cfg, Log: log, Store: db, Aggregator: aggregator,
		LB: lbController, FailOpen: failOpen, Incidents: incidents,
		Rollouts: rollouts, Registry: registry, Hub: hub,
		Directory: directory, Sessions: sessions, Syncer: syncer,
	})

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		evaluateLoop(ctx, log, db, aggregator, failOpen, lbController, probeRunner, hub)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		maintenanceLoop(ctx, log, db)
	}()

	err = server.Serve(ctx)
	wg.Wait()

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("stopped cleanly")
	return nil
}

// evaluateLoop is the heart of the pipeline: recompute health, decide whether
// the signal is trustworthy, and reweight only if it is.
func evaluateLoop(
	ctx context.Context,
	log *slog.Logger,
	db *store.Store,
	agg *health.Aggregator,
	failOpen *failopen.Controller,
	lbController *lb.Controller,
	runner *prober.Runner,
	hub *events.Hub,
) {
	ticker := time.NewTicker(evaluateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		snapshot, err := agg.Compute(ctx)
		if err != nil {
			log.Error("compute health snapshot", "error", err)
			continue
		}
		hub.Publish(events.TypeHealth, snapshot)

		regions, err := db.ListRegions(ctx)
		if err != nil {
			log.Error("list regions", "error", err)
			continue
		}

		decision, err := failOpen.Evaluate(ctx, regions, runner)
		if err != nil {
			log.Error("evaluate fail-open", "error", err)
			continue
		}

		scores := make(map[string]float64, len(snapshot.Regions))
		for _, r := range snapshot.Regions {
			scores[r.RegionID] = r.Score
		}

		if err := lbController.Reconcile(ctx, scores, decision.Trusted); err != nil {
			log.Error("reconcile weights", "error", err)
		}
	}
}

// maintenanceLoop rolls probe partitions forward and refreshes the rollup that
// backs the uptime trend.
func maintenanceLoop(ctx context.Context, log *slog.Logger, db *store.Store) {
	// Run once at startup so a fresh deployment has today's partition before
	// the first probe lands.
	runMaintenance(ctx, log, db)

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runMaintenance(ctx, log, db)
		}
	}
}

func runMaintenance(ctx context.Context, log *slog.Logger, db *store.Store) {
	if err := db.EnsurePartitions(ctx, 7); err != nil {
		log.Error("ensure probe partitions", "error", err)
	}
	if err := db.RefreshRollup(ctx); err != nil {
		log.Error("refresh probe rollup", "error", err)
	}
}

// serviceSyncer reconciles the probe fleet with the registered service list.
type serviceSyncer struct {
	store  *store.Store
	runner *prober.Runner
	// ctx is the process lifetime, used as the parent for probe goroutines so
	// they outlive the request that created them.
	ctx context.Context
}

// SyncNow reloads services and reconciles probe targets.
func (s *serviceSyncer) SyncNow(reqCtx context.Context) error {
	services, err := s.store.ListServices(reqCtx)
	if err != nil {
		return err
	}
	s.runner.Sync(s.ctx, services)
	return nil
}

// statusLogger records target status transitions.
type statusLogger struct{ log *slog.Logger }

// OnStatusChange implements prober.Notifier.
func (s *statusLogger) OnStatusChange(serviceID, regionID string, from, to models.Status) {
	s.log.Info("probe target transitioned",
		"service", serviceID, "region", regionID, "from", from, "to", to)
}

func buildDirectory(cfg *config.Config, log *slog.Logger) auth.Directory {
	if cfg.AuthDisabled {
		log.Warn("AUTH_DISABLED=true — all requests run as the dev user. Never use this in production.",
			"user", cfg.DevUser, "groups", cfg.DevGroups)
		return auth.StaticDirectory{User: cfg.DevUser, GroupList: cfg.DevGroups}
	}
	return auth.NewLDAPDirectory(cfg, log)
}

func logLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
