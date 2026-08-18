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
	"github.com/beravelli/health-checker-router/internal/leader"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/obs"
	"github.com/beravelli/health-checker-router/internal/prober"
	"github.com/beravelli/health-checker-router/internal/rollout"
	"github.com/beravelli/health-checker-router/internal/routing"
	"github.com/beravelli/health-checker-router/internal/routing/akamai"
	"github.com/beravelli/health-checker-router/internal/routing/arc"
	"github.com/beravelli/health-checker-router/internal/routing/avigslb"
	"github.com/beravelli/health-checker-router/internal/routing/awsnative"
	"github.com/beravelli/health-checker-router/internal/routing/cloudfrontadapter"
	"github.com/beravelli/health-checker-router/internal/store"
)

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

	// Election and metrics come first: the probe runner reports to metrics, and
	// the metrics exporter reads leadership at scrape time.
	elector := leader.New(db.Pool(), cfg.LeaderPollInterval, log)
	metrics := obs.New(elector)

	// Routing adapters. All four are registered; only the one enabled in
	// Admin is ever built, and building resolves its credentials.
	awsResolver := awsauth.NewResolver(cfg)
	registry := routing.NewRegistry()
	registry.Register(models.ProviderAWSNative, awsnative.Factory(awsResolver))
	registry.Register(models.ProviderCloudFront, cloudfrontadapter.Factory(awsResolver))
	registry.Register(models.ProviderAviGSLB, avigslb.Factory())
	registry.Register(models.ProviderAkamaiCloudlet, akamai.Factory())
	registry.Register(models.ProviderAWSARC, arc.Factory(awsResolver))

	lbController := lb.New(db, registry, hub, log)

	failOpen := failopen.New(failopen.Policy{
		QuorumRequired: cfg.QuorumRequired,
		QuorumTotal:    cfg.QuorumTotal,
		Hold:           cfg.FailOpenHold,
	}, db, incidents, hub, log)

	probeRunner := prober.NewRunner(cfg, db, &statusLogger{log: log}, metrics,
		awsauth.NewTargetHealth(awsResolver), log)
	defer probeRunner.Stop()

	aggregator := health.New(db, probeRunner)
	rollouts := rollout.New(db, probeRunner, hub, log)

	// Probe goroutines are parented to the leadership term, not the process, so
	// this starts empty and is populated when this replica is elected.
	syncer := &serviceSyncer{store: db, runner: probeRunner}

	directory := buildDirectory(cfg, log)
	sessions := auth.NewSessionStore(12 * time.Hour)

	server := api.New(api.Deps{
		Config: cfg, Log: log, Store: db, Aggregator: aggregator,
		LB: lbController, FailOpen: failOpen, Incidents: incidents,
		Rollouts: rollouts, Registry: registry, Hub: hub,
		Directory: directory, Sessions: sessions, Syncer: syncer,
		Metrics: metrics, Leader: elector,
	})

	var wg sync.WaitGroup

	// The probe fleet and the evaluate loop run on exactly one replica.
	//
	// Both are singletons by nature: probing from every replica multiplies load
	// on the targets, and reconciling from every replica means concurrent
	// writers racing on the same weighted records. Standby replicas still serve
	// the console and the read API — they just do not probe and do not decide.
	wg.Add(1)
	go func() {
		defer wg.Done()
		elector.Run(ctx, func(termCtx context.Context) {
			log.Info("elected leader — starting probe fleet and evaluate loop")

			// Probe goroutines are parented to the term, so losing the lock
			// stops them and the next leader starts its own.
			syncer.setProbeContext(termCtx)
			if err := syncer.SyncNow(termCtx); err != nil {
				log.Error("initial probe sync", "error", err)
			}

			evaluateLoop(termCtx, log, cfg, db, aggregator, failOpen, lbController, probeRunner, hub, metrics)

			probeRunner.Stop()
			log.Info("stepped down — probe fleet stopped")
		})
	}()

	// Maintenance is also leader-only: EnsurePartitions and RefreshRollup are
	// DDL and a full rollup rebuild, and running them from six replicas an hour
	// apart is pure contention.
	wg.Add(1)
	go func() {
		defer wg.Done()
		maintenanceLoop(ctx, log, db, elector)
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
	cfg *config.Config,
	db *store.Store,
	agg *health.Aggregator,
	failOpen *failopen.Controller,
	lbController *lb.Controller,
	runner *prober.Runner,
	hub *events.Hub,
	metrics *obs.Metrics,
) {
	ticker := time.NewTicker(cfg.EvaluateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		started := time.Now()

		snapshot, err := agg.Compute(ctx)
		if err != nil {
			log.Error("compute health snapshot", "error", err)
			metrics.EvaluationComplete("error", time.Since(started))
			continue
		}
		hub.Publish(events.TypeHealth, snapshot)

		regions, err := db.ListRegions(ctx)
		if err != nil {
			log.Error("list regions", "error", err)
			metrics.EvaluationComplete("error", time.Since(started))
			continue
		}

		decision, err := failOpen.Evaluate(ctx, regions, runner)
		if err != nil {
			log.Error("evaluate fail-open", "error", err)
			metrics.EvaluationComplete("error", time.Since(started))
			continue
		}

		scores := make(map[string]float64, len(snapshot.Regions))
		for _, r := range snapshot.Regions {
			scores[r.RegionID] = r.Score
			metrics.RegionScore(r.RegionID, r.Score)
		}
		for _, r := range regions {
			metrics.UnknownRatio(r.ID, runner.UnknownRatio(r.ID))
			metrics.RegionTrusted(r.ID, decision.Trusted[r.ID])
		}
		metrics.FailOpen(decision.FailOpen)

		// Weights are held, not recomputed, when the signal is inconclusive.
		// Recorded as a distinct outcome so a dashboard shows the difference
		// between "decided" and "declined to decide".
		if decision.FailOpen {
			metrics.EvaluationComplete("held", time.Since(started))
			continue
		}

		if err := lbController.Reconcile(ctx, scores, decision.Trusted); err != nil {
			log.Error("reconcile weights", "error", err)
			metrics.EvaluationComplete("error", time.Since(started))
			continue
		}

		if weights, err := db.GetWeights(ctx); err == nil {
			for _, w := range weights {
				metrics.RegionWeight(w.RegionID, float64(w.Weight))
			}
		}
		metrics.EvaluationComplete("applied", time.Since(started))
	}
}

// maintenanceLoop rolls probe partitions forward and refreshes the rollup that
// backs the uptime trend.
func maintenanceLoop(ctx context.Context, log *slog.Logger, db *store.Store, elector *leader.Elector) {
	// Run once at startup so a fresh deployment has today's partition before
	// the first probe lands. Guarded by leadership like the hourly pass: on a
	// standby this is a no-op until it is elected.
	if elector.IsLeader() {
		runMaintenance(ctx, log, db)
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !elector.IsLeader() {
				continue
			}
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

	// mu guards probeCtx, which is replaced on every leadership transition
	// while admin requests may be calling SyncNow concurrently.
	mu sync.RWMutex
	// probeCtx parents the probe goroutines. It is the leadership term, not the
	// request and not the process: probes must outlive the request that
	// registered a service, but must stop when this replica steps down.
	probeCtx context.Context
}

// setProbeContext installs the context probe goroutines are parented to.
// Called on election with the term context.
func (s *serviceSyncer) setProbeContext(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeCtx = ctx
}

// SyncNow reloads services and reconciles probe targets.
//
// On a standby replica there is no probe context, so this reloads nothing and
// reports success: the admin mutation that triggered it has already been
// persisted, and the leader will pick the change up on its own next sync. The
// alternative — a standby starting probe goroutines — is the duplicate-probing
// problem leadership exists to prevent.
func (s *serviceSyncer) SyncNow(reqCtx context.Context) error {
	s.mu.RLock()
	probeCtx := s.probeCtx
	s.mu.RUnlock()
	if probeCtx == nil {
		return nil
	}

	services, err := s.store.ListServices(reqCtx)
	if err != nil {
		return err
	}
	s.runner.Sync(probeCtx, services)
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
