package jobs

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// stubProgressStore wraps a noopStore and returns context.Canceled
// from UpdateProgress whenever the caller's ctx is already done. Used
// to exercise the shutdown race in tryFlushProgress without requiring
// goroutine timing.
type stubProgressStore struct {
	noopStore
	calls int
}

func (s *stubProgressStore) UpdateProgress(ctx context.Context, _ string, _, _ int64, _ string) error {
	s.calls++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func TestTryFlushProgress_RestoresDirtyOnShutdownRace(t *testing.T) {
	// Regression for the shutdown race: when tryFlushProgress fires
	// just before pfStop, it captures the snap and clears dirty
	// BEFORE the store call. If the store call then fails due to
	// ctx cancellation, the dirty flag must be put back so the
	// runner's final-flush picks up the snap. Otherwise the latest
	// Progress() call is silently dropped.
	store := &stubProgressStore{}
	m, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(m, WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	state := &jobState{
		jobID:         "j1",
		lastProgress:  Progress{Done: 50, Total: 100, Msg: "halfway"},
		progressDirty: true,
	}

	// Cancelled ctx mimics pfStop firing while tryFlushProgress was
	// already mid-call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.tryFlushProgress(ctx, state)

	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.progressDirty {
		t.Error("progressDirty = false after shutdown-race flush; the snap would be lost")
	}
	if state.lastProgress.Done != 50 || state.lastProgress.Total != 100 || state.lastProgress.Msg != "halfway" {
		t.Errorf("lastProgress mutated to %+v, want unchanged", state.lastProgress)
	}
}

func TestEligibleQueues_DropsSaturated(t *testing.T) {
	// Regression for the starvation bug: when a per-queue cap is hit,
	// that queue must be dropped from the Claim request entirely so
	// the SQL backends' SELECT LIMIT does not waste its budget on
	// rows that will be filtered out, starving the remaining queues.
	m, err := New(&noopStore{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(m, WorkerConfig{
		Queues:      []string{"high", "low"},
		Concurrency: 5,
		PerQueue:    map[string]int{"high": 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two jobs in-flight on "high" → at cap.
	w.bumpQueueInFlight("high", 2)

	queues, budget := w.eligibleQueues()
	if !reflect.DeepEqual(queues, []string{"low"}) {
		t.Errorf("queues = %v, want [low] (high is saturated)", queues)
	}
	if _, present := budget["high"]; present {
		t.Errorf("budget = %v, want no entry for saturated 'high'", budget)
	}

	// One slot frees up → "high" returns to the eligible set.
	w.bumpQueueInFlight("high", -1)
	queues, budget = w.eligibleQueues()
	if !reflect.DeepEqual(queues, []string{"high", "low"}) {
		t.Errorf("queues = %v, want [high low] after slot freed", queues)
	}
	if budget["high"] != 1 {
		t.Errorf("budget[high] = %d, want 1", budget["high"])
	}
}

// noopStore satisfies the Store interface with no-op methods. Tests
// embed it and override only the methods they care about; named
// receiver and parameters make the override boilerplate copy-paste
// friendly.
type noopStore struct{}

func (s *noopStore) Insert(ctx context.Context, row *JobRow) error {
	return nil
}
func (s *noopStore) InsertTx(ctx context.Context, tx any, row *JobRow) error {
	return ErrUnsupported
}
func (s *noopStore) Get(ctx context.Context, id string) (*JobRow, error) {
	return nil, ErrNotFound
}
func (s *noopStore) List(ctx context.Context, f JobFilter) ([]*JobRow, string, error) {
	return nil, "", nil
}
func (s *noopStore) Claim(ctx context.Context, req ClaimRequest) ([]*JobRow, error) {
	return nil, nil
}
func (s *noopStore) Heartbeat(ctx context.Context, jobID, workerID string, until time.Time) (bool, error) {
	return false, ErrNotFound
}
func (s *noopStore) Complete(ctx context.Context, jobID, workerID string, o Outcome) (State, error) {
	return "", ErrNotFound
}
func (s *noopStore) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	return 0, nil
}
func (s *noopStore) RecordAttempt(ctx context.Context, a *Attempt) error {
	return nil
}
func (s *noopStore) ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]*Attempt, error) {
	return nil, nil
}
func (s *noopStore) GetStep(ctx context.Context, jobID, name string) (*StepRecord, error) {
	return nil, ErrNotFound
}
func (s *noopStore) SaveStep(ctx context.Context, r *StepRecord) error {
	return nil
}
func (s *noopStore) ListSteps(ctx context.Context, jobID string) ([]*StepRecord, error) {
	return nil, nil
}
func (s *noopStore) UpdateProgress(ctx context.Context, jobID string, done, total int64, msg string) error {
	return nil
}
func (s *noopStore) Retry(ctx context.Context, jobID string, now time.Time) error {
	return nil
}
func (s *noopStore) Cancel(ctx context.Context, jobID string, now time.Time) (bool, error) {
	return false, nil
}
func (s *noopStore) Delete(ctx context.Context, jobID string) error {
	return nil
}
func (s *noopStore) UpsertWorker(ctx context.Context, w *WorkerRow) error {
	return nil
}
func (s *noopStore) RetireWorker(ctx context.Context, workerID string) error {
	return nil
}
func (s *noopStore) ListWorkers(ctx context.Context) ([]*WorkerRow, error) {
	return nil, nil
}
func (s *noopStore) SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error) {
	return 0, nil
}
func (s *noopStore) ListQueues(ctx context.Context) ([]QueueInfo, error) {
	return nil, nil
}
func (s *noopStore) UpsertSchedule(ctx context.Context, sched *ScheduleRow) error {
	return nil
}
func (s *noopStore) DeleteSchedule(ctx context.Context, name string) error {
	return nil
}
func (s *noopStore) ListSchedules(ctx context.Context) ([]*ScheduleRow, error) {
	return nil, nil
}
func (s *noopStore) DueSchedules(ctx context.Context, now time.Time) ([]*ScheduleRow, error) {
	return nil, nil
}
func (s *noopStore) ClaimSchedule(ctx context.Context, name string, expectedLast, newLast, newNext time.Time) (bool, error) {
	return false, nil
}
