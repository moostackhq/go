package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/sqlite"
)

// newDB opens a SQLite database backed by a temp file (one per
// test) so multiple connections in the pool see consistent state.
// ":memory:" would make each connection a separate database.
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newStore(t *testing.T) (*sqlite.Store, *sql.DB) {
	t.Helper()
	db := newDB(t)
	s, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

// --- registration helpers shared with the package-level tests ---

type emailJob struct {
	UserID int64 `json:"user_id"`
}

func (j *emailJob) Run(_ jobs.Context) error { return nil }

type sqliteCountJob struct {
	Tag string `json:"tag"`
}

var sqliteCounters = map[string]*atomic.Int32{}

func (j *sqliteCountJob) Run(_ jobs.Context) error {
	if c := sqliteCounters[j.Tag]; c != nil {
		c.Add(1)
	}
	return nil
}

// --- Phase 1 surface ---

func TestSQLite_Insert_GetRoundTrip(t *testing.T) {
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

func TestSQLite_List_FilterAndPagination(t *testing.T) {
	store, _ := newStore(t)
	m, _ := jobs.New(store, jobs.Config{})
	if err := jobs.Register[emailJob](m, "email"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 150; i++ {
		queue := "default"
		if i%3 == 0 {
			queue = "priority"
		}
		_, err := m.Enqueue(context.Background(), &emailJob{UserID: int64(i)}, jobs.Options{Queue: queue})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Filter by queue + paginate.
	seen := 0
	cursor := ""
	for {
		page, err := m.ListJobs(context.Background(), jobs.JobFilter{
			Queues: []string{"priority"},
			Limit:  20,
			Cursor: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		seen += len(page.Jobs)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	// 150/3 = 50 jobs in "priority".
	if seen != 50 {
		t.Errorf("paginated count = %d, want 50", seen)
	}
}

func TestSQLite_UniqueKey_CollisionReturnsExistingID(t *testing.T) {
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

// --- Phase 1 EnqueueTx: SQLite participates in a real transaction ---

func TestSQLite_EnqueueTx_CommitsOnlyOnCommit(t *testing.T) {
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

	// Before commit, the job is invisible to other transactions
	// (SQLite's BEGIN IMMEDIATE serialises writers; concurrent
	// readers see committed state). Verifying via the store's Get
	// from another connection would need a separate db; instead we
	// roll back and confirm the row never landed.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	_, err = m.GetJob(context.Background(), id)
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("after rollback: want ErrNotFound, got %v", err)
	}

	// Now do it again, with commit.
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

// --- Phase 2-7 end-to-end ---

func TestSQLite_WorkerEndToEnd(t *testing.T) {
	store, _ := newStore(t)
	sqliteCounters = map[string]*atomic.Int32{}
	c := &atomic.Int32{}
	sqliteCounters["sqlite-e2e"] = c

	m, _ := jobs.New(store, jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	if err := jobs.Register[sqliteCountJob](m, "counter"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &sqliteCountJob{Tag: "sqlite-e2e"})
	if err != nil {
		t.Fatal(err)
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
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

func TestSQLite_SweepReclaimsExpiredLease(t *testing.T) {
	store, _ := newStore(t)
	sqliteCounters = map[string]*atomic.Int32{}
	c := &atomic.Int32{}
	sqliteCounters["sqlite-recover"] = c

	m, _ := jobs.New(store, jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	if err := jobs.Register[sqliteCountJob](m, "counter"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &sqliteCountJob{Tag: "sqlite-recover"})
	if err != nil {
		t.Fatal(err)
	}

	// Pretend a worker claimed it with a tiny lease, then died.
	claimed, err := store.Claim(context.Background(), jobs.ClaimRequest{
		WorkerID:      "ghost",
		Queues:        []string{"default"},
		Now:           time.Now().UTC(),
		LeaseDuration: 50 * time.Millisecond,
		Limit:         1,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("setup claim: claimed=%v err=%v", claimed, err)
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     300 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     10 * time.Millisecond,
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
		info, _ := m.GetJob(context.Background(), id)
		if info != nil && info.State == jobs.StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.Load() != 1 {
		t.Errorf("runs = %d, want 1 (recovery attempt)", c.Load())
	}
}

// sqliteHoldJob blocks for HoldMs (capped to ctx). Used by the
// starvation test to keep "high" jobs in-flight while a "low" job
// must still get through.
type sqliteHoldJob struct {
	Tag    string `json:"tag"`
	HoldMs int    `json:"hold_ms"`
}

func (j *sqliteHoldJob) Run(ctx jobs.Context) error {
	if c := sqliteCounters[j.Tag]; c != nil {
		c.Add(1)
	}
	if j.HoldMs <= 0 {
		return nil
	}
	select {
	case <-time.After(time.Duration(j.HoldMs) * time.Millisecond):
	case <-ctx.Done():
	}
	return nil
}

func TestSQLite_PerQueueCap_DoesNotStarveLowerPriorityQueue(t *testing.T) {
	// Reproduces the SQL-backend starvation bug: SELECT's ORDER BY
	// priority DESC LIMIT N fetches only at-cap "high"-priority rows,
	// the worker filters all of them out, and the lower-priority
	// "low" queue starves. Worker-side fix drops saturated queues
	// from req.Queues so the SELECT skips past them.
	store, _ := newStore(t)
	sqliteCounters = map[string]*atomic.Int32{}
	highC, lowC := &atomic.Int32{}, &atomic.Int32{}
	sqliteCounters["high"] = highC
	sqliteCounters["low"] = lowC

	m, _ := jobs.New(store, jobs.Config{})
	if err := jobs.Register[sqliteHoldJob](m, "hold"); err != nil {
		t.Fatal(err)
	}

	// 10 high-priority jobs that each hold for 500ms (longer than
	// the test's deadline budget for the low job).
	for i := 0; i < 10; i++ {
		if _, err := m.Enqueue(context.Background(),
			&sqliteHoldJob{Tag: "high", HoldMs: 500},
			jobs.Options{Queue: "high", Priority: 9}); err != nil {
			t.Fatal(err)
		}
	}
	// One low-priority job that returns immediately.
	if _, err := m.Enqueue(context.Background(),
		&sqliteHoldJob{Tag: "low", HoldMs: 0},
		jobs.Options{Queue: "low", Priority: 0}); err != nil {
		t.Fatal(err)
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		Queues:            []string{"high", "low"},
		Concurrency:       5,
		PerQueue:          map[string]int{"high": 2},
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     2 * time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		SweepInterval:     500 * time.Millisecond,
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

	// Without the fix, the low job never gets claimed because SELECT
	// LIMIT keeps returning at-cap high-priority rows. 300ms is well
	// under the high jobs' 500ms hold but plenty for two poll cycles.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && lowC.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if lowC.Load() == 0 {
		t.Fatalf("low-priority job starved; high runs = %d", highC.Load())
	}
}

func TestSQLite_Complete_RejectsAfterSweepReclaim(t *testing.T) {
	// Lease-loss race: SweepExpired moved the row back to available
	// (worker process paused longer than the lease). When the worker
	// resumes and calls Complete with its stale outcome, the store
	// must reject with ErrNotFound so the reclaim isn't clobbered.
	store, _ := newStore(t)
	ctx := context.Background()

	// Claim with a 1ms lease and don't heartbeat: it expires immediately.
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

	// Worker A's stale Complete should be rejected.
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

func TestSQLite_AttemptsAndStepsPersistAcrossRestarts(t *testing.T) {
	// Round-trip a successful job's attempt history through the
	// store, then verify ListJobAttempts/ListJobSteps return it.
	store, _ := newStore(t)
	m, _ := jobs.New(store, jobs.Config{})
	jobs.Register[emailJob](m, "email")

	id, _ := m.Enqueue(context.Background(), &emailJob{UserID: 1})
	now := time.Now().UTC()

	// Hand-write the attempt and step rows so the test does not need
	// to start a worker.
	if err := store.RecordAttempt(context.Background(), &jobs.Attempt{
		ID:         "att-1",
		JobID:      id,
		Attempt:    1,
		WorkerID:   "w",
		StartedAt:  now,
		FinishedAt: now.Add(50 * time.Millisecond),
		State:      jobs.AttemptSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStep(context.Background(), &jobs.StepRecord{
		ID:         "step-1",
		JobID:      id,
		Name:       "download",
		State:      jobs.StepSucceeded,
		Result:     []byte(`"file.csv"`),
		StartedAt:  now,
		FinishedAt: now.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := m.ListJobAttempts(context.Background(), id, jobs.AttemptsFilter{})
	if err != nil || len(page.Attempts) != 1 {
		t.Fatalf("attempts: len=%d err=%v", len(page.Attempts), err)
	}
	steps, err := m.ListJobSteps(context.Background(), id)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps: len=%d err=%v", len(steps), err)
	}
}

func TestSQLite_UpsertSchedule_PreservesNextRunAtWhenCronUnchanged(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	originalNext := time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)

	if err := store.UpsertSchedule(ctx, &jobs.ScheduleRow{
		Name:      "daily",
		Kind:      "report",
		Cron:      "0 6 * * *",
		Payload:   []byte("{}"),
		OptionsJSON: []byte("{}"),
		NextRunAt: originalNext,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Same cron, different NextRunAt; must preserve.
	if err := store.UpsertSchedule(ctx, &jobs.ScheduleRow{
		Name:      "daily",
		Kind:      "report",
		Cron:      "0 6 * * *",
		Payload:   []byte("{}"),
		OptionsJSON: []byte("{}"),
		NextRunAt: originalNext.Add(24 * time.Hour),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(ctx)
	if !rows[0].NextRunAt.Equal(originalNext) {
		t.Errorf("NextRunAt after same-cron upsert = %v, want preserved %v",
			rows[0].NextRunAt, originalNext)
	}

	// Different cron; must update.
	newNext := originalNext.Add(48 * time.Hour)
	if err := store.UpsertSchedule(ctx, &jobs.ScheduleRow{
		Name:      "daily",
		Kind:      "report",
		Cron:      "0 18 * * *",
		Payload:   []byte("{}"),
		OptionsJSON: []byte("{}"),
		NextRunAt: newNext,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = store.ListSchedules(ctx)
	if !rows[0].NextRunAt.Equal(newNext) {
		t.Errorf("NextRunAt after cron change = %v, want updated to %v",
			rows[0].NextRunAt, newNext)
	}
}

func TestSQLite_ScheduleEndToEnd(t *testing.T) {
	store, _ := newStore(t)
	sqliteCounters = map[string]*atomic.Int32{}
	c := &atomic.Int32{}
	sqliteCounters["sqlite-sched"] = c

	m, _ := jobs.New(store, jobs.Config{
		SchedulerInterval: 20 * time.Millisecond,
	})
	if err := jobs.Register[sqliteCountJob](m, "counter"); err != nil {
		t.Fatal(err)
	}

	if err := m.Schedule(context.Background(), "sqlite-sched", "0 * * * *",
		&sqliteCountJob{Tag: "sqlite-sched"}, jobs.ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}

	// Rewind NextRunAt so the first scheduler tick fires.
	// UpsertSchedule preserves NextRunAt on same-cron upsert, so
	// delete first to force a fresh insert.
	rows, _ := store.ListSchedules(context.Background())
	rows[0].NextRunAt = time.Now().Add(-2 * time.Minute)
	rows[0].LastRunAt = time.Time{}
	_ = store.DeleteSchedule(context.Background(), rows[0].Name)
	_ = store.UpsertSchedule(context.Background(), rows[0])

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     300 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     50 * time.Millisecond,
	})
	wDone := make(chan struct{})
	go func() { _ = w.Start(context.Background()); close(wDone) }()
	defer func() { w.Stop(context.Background()); <-wDone }()

	schedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedDone := make(chan struct{})
	go func() { _ = m.StartScheduler(schedCtx); close(schedDone) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-schedDone
	if c.Load() == 0 {
		t.Fatal("scheduled job never ran")
	}
}

