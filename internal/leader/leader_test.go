//go:build integration

// Leader election is tested against a real Postgres, not a fake.
//
// The whole mechanism is the database's advisory-lock semantics — session
// scoping, automatic release when a connection dies, mutual exclusion across
// processes. A mock would only assert that we call pg_try_advisory_lock, which
// is the one part that cannot be wrong. Run with:
//
//	go test -tags=integration ./internal/leader/
//
// TEST_DATABASE_URL must point at a disposable database.
package leader

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	// Each elector holds a connection for its whole term, so the pool must be
	// able to supply one per concurrent candidate plus the watchdog pings.
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOnlyOneLeaderAcrossCandidates is the guarantee the package exists for:
// with five candidates competing, exactly one runs the pipeline.
func TestOnlyOneLeaderAcrossCandidates(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const candidates = 5
	var (
		concurrent atomic.Int32 // how many are inside onElected right now
		maxSeen    atomic.Int32
		elections  atomic.Int32
		wg         sync.WaitGroup
	)

	for i := 0; i < candidates; i++ {
		e := New(pool, 50*time.Millisecond, discardLog())
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(ctx, func(termCtx context.Context) {
				n := concurrent.Add(1)
				defer concurrent.Add(-1)
				elections.Add(1)
				for {
					if prev := maxSeen.Load(); n > prev {
						if maxSeen.CompareAndSwap(prev, n) {
							break
						}
						continue
					}
					break
				}
				// Hold the term until the test ends.
				<-termCtx.Done()
			})
		}()
	}

	// Give the field time to settle and to retry a few times.
	time.Sleep(600 * time.Millisecond)

	if got := maxSeen.Load(); got != 1 {
		t.Errorf("%d candidates ran the pipeline concurrently; want exactly 1", got)
	}
	if got := elections.Load(); got != 1 {
		t.Errorf("%d candidates were elected; want exactly 1", got)
	}

	cancel()
	wg.Wait()
}

// TestFailoverOnLeaderExit checks that stepping down hands leadership on,
// which is what makes a rolling deploy safe.
func TestFailoverOnLeaderExit(t *testing.T) {
	pool := testPool(t)

	leaderCtx, stopLeader := context.WithCancel(context.Background())
	standbyCtx, stopStandby := context.WithCancel(context.Background())
	defer stopStandby()

	first := New(pool, 50*time.Millisecond, discardLog())
	second := New(pool, 50*time.Millisecond, discardLog())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); first.Run(leaderCtx, func(c context.Context) { <-c.Done() }) }()
	// Let the first one win deterministically.
	time.Sleep(200 * time.Millisecond)
	go func() { defer wg.Done(); second.Run(standbyCtx, func(c context.Context) { <-c.Done() }) }()
	time.Sleep(200 * time.Millisecond)

	if !first.IsLeader() {
		t.Fatal("first candidate should hold leadership")
	}
	if second.IsLeader() {
		t.Fatal("second candidate should be standby")
	}

	// The leader exits, as during a rolling deploy.
	stopLeader()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if second.IsLeader() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !second.IsLeader() {
		t.Fatal("standby did not take over after the leader exited")
	}
	if first.IsLeader() {
		t.Error("the departed leader still reports leadership")
	}

	stopStandby()
	wg.Wait()
}

// TestLockReleasedAfterRun guards against leaking the advisory lock, which
// would deadlock the fleet permanently — no future replica could ever be
// elected.
func TestLockReleasedAfterRun(t *testing.T) {
	pool := testPool(t)

	ctx, cancel := context.WithCancel(context.Background())
	e := New(pool, 50*time.Millisecond, discardLog())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx, func(c context.Context) { <-c.Done() })
	}()

	time.Sleep(200 * time.Millisecond)
	if !e.IsLeader() {
		t.Fatal("expected leadership")
	}
	cancel()
	<-done

	// The lock must be gone from pg_locks, not merely forgotten locally.
	var held bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		   SELECT 1 FROM pg_locks
		   WHERE locktype = 'advisory'
		     AND ((classid::bigint << 32) | objid::bigint) = $1
		 )`, LockID).Scan(&held)
	if err != nil {
		t.Fatalf("inspect pg_locks: %v", err)
	}
	if held {
		t.Error("advisory lock still held after Run returned")
	}
}

func TestTermCounterIncrements(t *testing.T) {
	pool := testPool(t)
	e := New(pool, 50*time.Millisecond, discardLog())

	for want := int64(1); want <= 2; want++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); e.Run(ctx, func(c context.Context) { <-c.Done() }) }()
		time.Sleep(200 * time.Millisecond)
		if got := e.Term(); got != want {
			t.Errorf("term = %d, want %d", got, want)
		}
		cancel()
		<-done
		if e.IsLeader() {
			t.Error("still leader after Run returned")
		}
	}
}
