package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/postgres"
)

// connect opens a DB against $JOBS_PG_URL or skips the test.
// The URL must already point at a database that is safe to alter
// (tests apply the schema and write rows).
func connect(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("JOBS_PG_URL")
	if dsn == "" {
		t.Skip("set JOBS_PG_URL to run Postgres tests (e.g. postgres://localhost/jobs_test?sslmode=disable)")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("cannot connect to Postgres at %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// reset wipes the tables so tests can run in any order without
// cross-contamination. The schema is left intact.
func reset(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"job_attempts", "job_steps", "schedules", "workers", "jobs"} {
		if _, err := db.ExecContext(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
}

func newStore(t *testing.T) (*postgres.Store, *sql.DB) {
	t.Helper()
	db := connect(t)
	s, err := postgres.New(db, postgres.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	reset(t, db)
	return s, db
}

// --- fixtures ---

type emailJob struct {
	UserID int64 `json:"user_id"`
}

func (j *emailJob) Run(_ jobs.Context) error { return nil }

type pgCountJob struct {
	Tag string `json:"tag"`
}

var pgCounters = map[string]*atomic.Int32{}

func (j *pgCountJob) Run(_ jobs.Context) error {
	if c := pgCounters[j.Tag]; c != nil {
		c.Add(1)
	}
	return nil
}

// --- tests ---

func TestPostgres_Insert_GetRoundTrip(t *testing.T) {
	store, _ := newStore(t)
	m, _ := jobs.New(store, jobs.Config{})
	if err := jobs.Register[emailJob](m, "email"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &emailJob{UserID: 42})
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "email" || info.State != jobs.StateAvailable {
		t.Errorf("info = %+v", info)
	}
}

func TestPostgres_UniqueKey_CollisionReturnsExistingID(t *testing.T) {
	store, _ := newStore(t)
	m, _ := jobs.New(store, jobs.Config{})
	if err := jobs.Register[emailJob](m, "email"); err != nil {
		t.Fatal(err)
	}
	first, err := m.Enqueue(context.Background(), &emailJob{UserID: 1}, jobs.Options{
		UniqueKey: "import:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Enqueue(context.Background(), &emailJob{UserID: 2}, jobs.Options{
		UniqueKey: "import:1",
	})
	if !errors.Is(err, jobs.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if second != first {
		t.Errorf("duplicate returned id %q, want %q", second, first)
	}
}

func TestPostgres_EnqueueTx_CommitsOnlyOnCommit(t *testing.T) {
	store, db := newStore(t)
	m, _ := jobs.New(store, jobs.Config{})
	if err := jobs.Register[emailJob](m, "email"); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.EnqueueTx(context.Background(), tx, &emailJob{UserID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	_, err = m.GetJob(context.Background(), id)
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("after rollback: want ErrNotFound, got %v", err)
	}

	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := m.EnqueueTx(context.Background(), tx, &emailJob{UserID: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetJob(context.Background(), id2); err != nil {
		t.Errorf("after commit: %v", err)
	}
}

func TestPostgres_WorkerEndToEnd(t *testing.T) {
	store, _ := newStore(t)
	pgCounters = map[string]*atomic.Int32{}
	c := &atomic.Int32{}
	pgCounters["pg-e2e"] = c

	m, _ := jobs.New(store, jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	if err := jobs.Register[pgCountJob](m, "counter"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &pgCountJob{Tag: "pg-e2e"})
	if err != nil {
		t.Fatal(err)
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      10 * time.Millisecond,
		LeaseDuration:     300 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     50 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		_ = w.Start(context.Background())
		close(done)
	}()
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, err := m.GetJob(context.Background(), id)
		if err == nil && info.State == jobs.StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.Load() != 1 {
		t.Errorf("runs = %d, want 1", c.Load())
	}
}

func TestPostgres_SkipLocked_TwoWorkersClaimDisjointSets(t *testing.T) {
	// Two workers polling the same queue must claim disjoint sets
	// thanks to FOR UPDATE SKIP LOCKED. We verify by enqueueing N
	// gated jobs, letting both workers grab as many as they can,
	// and asserting no overlap.
	store, _ := newStore(t)
	pgCounters = map[string]*atomic.Int32{}
	c := &atomic.Int32{}
	pgCounters["skip-locked"] = c

	m, _ := jobs.New(store, jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	if err := jobs.Register[pgCountJob](m, "counter"); err != nil {
		t.Fatal(err)
	}
	const N = 30
	for i := 0; i < N; i++ {
		if _, err := m.Enqueue(context.Background(), &pgCountJob{Tag: "skip-locked"}); err != nil {
			t.Fatal(err)
		}
	}

	mkWorker := func() *jobs.Worker {
		w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
			Concurrency:       4,
			PollInterval:      10 * time.Millisecond,
			LeaseDuration:     500 * time.Millisecond,
			HeartbeatInterval: 100 * time.Millisecond,
			SweepInterval:     100 * time.Millisecond,
		})
		return w
	}
	w1, w2 := mkWorker(), mkWorker()
	d1 := make(chan struct{})
	d2 := make(chan struct{})
	go func() { _ = w1.Start(context.Background()); close(d1) }()
	go func() { _ = w2.Start(context.Background()); close(d2) }()
	defer func() {
		w1.Stop(context.Background())
		w2.Stop(context.Background())
		<-d1
		<-d2
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.Load() < N {
		time.Sleep(20 * time.Millisecond)
	}
	if c.Load() != N {
		t.Errorf("runs = %d, want %d", c.Load(), N)
	}
}

func TestPostgres_Complete_RejectsAfterSweepReclaim(t *testing.T) {
	// Lease-loss race: SweepExpired moved the row back to available
	// (worker process paused longer than the lease). When the worker
	// resumes and calls Complete with its stale outcome, the store
	// must reject with ErrNotFound so the reclaim isn't clobbered.
	store, _ := newStore(t)
	ctx := context.Background()

	m, _ := jobs.New(store, jobs.Config{})
	if err := jobs.Register[emailJob](m, "email"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(ctx, &emailJob{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, jobs.ClaimRequest{
		WorkerID:      "worker-A",
		Queues:        []string{"default"},
		Now:           time.Now().UTC(),
		LeaseDuration: time.Millisecond,
		Limit:         1,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("setup claim: claimed=%v err=%v", claimed, err)
	}
	time.Sleep(10 * time.Millisecond)

	reclaimed, err := store.SweepExpired(ctx, time.Now().UTC())
	if err != nil || reclaimed != 1 {
		t.Fatalf("SweepExpired: reclaimed=%d err=%v", reclaimed, err)
	}

	backoffTarget := time.Now().Add(10 * time.Minute).UTC()
	_, err = store.Complete(ctx, id, "worker-A", jobs.Outcome{
		State:       jobs.StateScheduled,
		Attempt:     1,
		AvailableAt: backoffTarget,
		Error:       "context canceled",
	})
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Complete after sweep: err = %v, want ErrNotFound", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != jobs.StateAvailable {
		t.Errorf("state = %s, want available", got.State)
	}
	if got.AvailableAt.Equal(backoffTarget) {
		t.Error("available_at was clobbered by stale Complete to backoff target")
	}
}
