// Package leader provides single-writer election over Postgres advisory locks.
//
// The problem it solves: the evaluate loop probes targets and then writes
// load-balancer weights through the routing provider. Run it in every replica
// and you get N times the probe load on every target and N concurrent writers
// racing on the same weighted records — with an HPA scaling to 6, six processes
// independently deciding where production traffic goes. Nothing about the
// pipeline is idempotent enough for that to be safe.
//
// Election, not coordination. One replica holds the lock and runs the loop; the
// others sit in standby, serving reads and the console. If the leader dies its
// Postgres session ends, the lock is released by the server, and a standby takes
// over within its poll interval.
//
// Why an advisory lock rather than a Kubernetes lease: the database is already a
// hard dependency and a single point of consistency, whereas the Kubernetes API
// is neither, and a leader that cannot reach Postgres cannot do its job anyway.
// pg_advisory_lock is session-scoped, so release on crash is the server's
// problem, not ours — there is no lease to expire and no clock to trust.
package leader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LockID namespaces the advisory lock.
//
// Advisory locks share one cluster-wide 64-bit keyspace, so this must not
// collide with another application's. Derived from
// hashtext('health-checker-router/evaluate') and then fixed as a constant,
// because a value computed at runtime would change if the string ever did and
// silently elect two leaders during the rollout that changed it.
const LockID int64 = 0x48434845_56414c31 // "HCHEVAL1"

// Elector campaigns for leadership and reports whether it currently holds it.
type Elector struct {
	pool     *pgxpool.Pool
	interval time.Duration
	log      *slog.Logger

	// held is read concurrently by /readyz, the metrics exporter and the
	// evaluate loop, so it is atomic rather than mutex-guarded.
	held atomic.Bool
	// term counts how many times this process has acquired leadership. Useful
	// in metrics: a term counter climbing across the fleet means the lock is
	// flapping, which usually means database connection churn.
	term atomic.Int64
}

// New constructs an Elector. interval is how often a standby retries.
func New(pool *pgxpool.Pool, interval time.Duration, log *slog.Logger) *Elector {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Elector{pool: pool, interval: interval, log: log}
}

// IsLeader reports whether this process currently holds the lock.
func (e *Elector) IsLeader() bool { return e.held.Load() }

// Term reports how many times this process has become leader.
func (e *Elector) Term() int64 { return e.term.Load() }

// Run campaigns until ctx is cancelled.
//
// While leadership is held, onElected runs. When it returns — because ctx was
// cancelled, or because the connection holding the lock broke — the lock is
// released and the loop goes back to campaigning.
//
// onElected must respect ctx cancellation. It is called at most once per term.
func (e *Elector) Run(ctx context.Context, onElected func(context.Context)) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		acquired, err := e.campaign(ctx, onElected)
		if err != nil && !errors.Is(err, context.Canceled) {
			e.log.Error("leader campaign failed", "error", err)
		}
		if acquired {
			// A term just ended. Retry immediately rather than waiting out the
			// interval, since this process may be the only candidate left.
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// campaign attempts one acquisition. It reports whether leadership was held.
//
// The lock is taken on a connection acquired for the whole term and never
// returned to the pool while it is held: pg_advisory_lock is session-scoped, so
// handing the connection back would let another goroutine — or the pool's
// health check — release it out from under us.
func (e *Elector) campaign(ctx context.Context, onElected func(context.Context)) (bool, error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", LockID).Scan(&got); err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !got {
		return false, nil
	}

	e.held.Store(true)
	termNo := e.term.Add(1)
	e.log.Info("acquired leadership", "term", termNo)

	// Cancelled when the term ends for any reason, so onElected stops with us.
	termCtx, endTerm := context.WithCancel(ctx)
	defer endTerm()

	// Watch the connection. If it dies the lock is already gone server-side,
	// and continuing to act as leader would mean two writers.
	go e.watch(termCtx, conn, endTerm)

	onElected(termCtx)

	e.held.Store(false)
	// Release explicitly on clean exit. On a broken connection this will fail,
	// which is harmless — the server has already dropped the lock with the
	// session.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", LockID); err != nil {
		e.log.Warn("release advisory lock", "error", err)
	}
	e.log.Info("released leadership", "term", termNo)
	return true, nil
}

// watch pings the lock-holding connection and ends the term if it fails.
func (e *Elector) watch(ctx context.Context, conn *pgxpool.Conn, endTerm func()) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := conn.Conn().Ping(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			e.log.Error("lost the connection holding leadership; ending term", "error", err)
			endTerm()
			return
		}
	}
}
