package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

func TestInsertTx_ReturnsUnsupported(t *testing.T) {
	s := memory.New()
	err := s.InsertTx(context.Background(), nil, &jobs.JobRow{
		ID:   "x",
		Kind: "k",
	})
	if !errors.Is(err, jobs.ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

func TestInsert_DuplicateID(t *testing.T) {
	s := memory.New()
	row := &jobs.JobRow{ID: "x", Kind: "k", State: jobs.StateAvailable, CreatedAt: time.Now()}
	if err := s.Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(context.Background(), row); err == nil {
		t.Error("expected error on duplicate ID")
	}
}

func TestInsert_UniqueKeyAllowedAfterTerminal(t *testing.T) {
	// A new job with the same (kind, unique_key) is allowed once the
	// previous one has reached a terminal state. This matches the
	// partial-unique-index semantics in the SQL schemas.
	s := memory.New()
	first := &jobs.JobRow{
		ID:        "1",
		Kind:      "k",
		UniqueKey: "u",
		State:     jobs.StateSucceeded, // terminal
		CreatedAt: time.Now(),
	}
	if err := s.Insert(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := &jobs.JobRow{
		ID:        "2",
		Kind:      "k",
		UniqueKey: "u",
		State:     jobs.StateAvailable,
		CreatedAt: time.Now(),
	}
	if err := s.Insert(context.Background(), second); err != nil {
		t.Errorf("second insert blocked by terminal first: %v", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := memory.New()
	_, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestComplete_CancelRequestedOverridesSucceeded(t *testing.T) {
	// The cancel-then-success race: CancelJob set cancel_requested
	// on a running row, then Run completed successfully before any
	// heartbeat observed the flag. Complete must rewrite the
	// outcome to Cancelled so the user's cancellation isn't lost.
	s := memory.New()
	row := &jobs.JobRow{
		ID:              "x",
		Kind:            "k",
		State:           jobs.StateRunning,
		LockedBy:        "w",
		LockedUntil:     time.Now().Add(time.Minute),
		CancelRequested: true,
		MaxAttempts:     3,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := s.Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	applied, err := s.Complete(context.Background(), "x", "w", jobs.Outcome{
		State:   jobs.StateSucceeded,
		Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied != jobs.StateCancelled {
		t.Errorf("applied = %s, want cancelled (override)", applied)
	}
	got, _ := s.Get(context.Background(), "x")
	if got.State != jobs.StateCancelled {
		t.Errorf("row state after Complete = %s, want cancelled", got.State)
	}
}

func TestComplete_RejectsAfterSweepReclaim(t *testing.T) {
	// Lease-loss race: SweepExpired reclaimed a row while the
	// worker's process was paused. When the worker resumes and
	// calls Complete, the row is state='available' (sweep set it)
	// with the lease cleared. Complete must reject with
	// ErrNotFound; otherwise the worker's stale outcome would
	// clobber the sweep's reclaim (push available_at into the
	// future, undo the attempt bump).
	s := memory.New()
	ctx := context.Background()

	// Set up a row whose lease has expired.
	now := time.Now().UTC()
	row := &jobs.JobRow{
		ID:          "z",
		Kind:        "k",
		State:       jobs.StateRunning,
		LockedBy:    "worker-A",
		LockedUntil: now.Add(-time.Second), // already expired
		HeartbeatAt: now.Add(-5 * time.Second),
		Attempt:     0,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := s.SweepExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 1 {
		t.Fatalf("SweepExpired = %d, want 1", reclaimed)
	}

	// Worker A resumes after the pause and tries to Complete.
	// Outcome reflects its stale view (attempt=1 from row.Attempt+1
	// at claim time; would-be-retry with a backoff delay).
	backoffTarget := now.Add(10 * time.Minute)
	_, err = s.Complete(ctx, "z", "worker-A", jobs.Outcome{
		State:       jobs.StateScheduled,
		Attempt:     1,
		AvailableAt: backoffTarget,
		Error:       "context canceled",
	})
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Complete after sweep: err = %v, want ErrNotFound", err)
	}

	// Row should still reflect what sweep wrote: state=available,
	// attempt=1, no lease, no future available_at.
	got, err := s.Get(ctx, "z")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != jobs.StateAvailable {
		t.Errorf("state = %s, want available (sweep set it)", got.State)
	}
	if got.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (sweep bumped it)", got.Attempt)
	}
	if got.LockedBy != "" {
		t.Errorf("locked_by = %q, want empty", got.LockedBy)
	}
	if got.AvailableAt.Equal(backoffTarget) {
		t.Error("available_at was clobbered by stale Complete to backoff target")
	}
}

func TestComplete_CancelRequestedLeavesNonSuccessAlone(t *testing.T) {
	// The override is narrowly scoped: only StateSucceeded gets
	// rewritten. A failed/discarded outcome with cancel_requested
	// set still lands as failed/discarded.
	s := memory.New()
	row := &jobs.JobRow{
		ID:              "y",
		Kind:            "k",
		State:           jobs.StateRunning,
		LockedBy:        "w",
		LockedUntil:     time.Now().Add(time.Minute),
		CancelRequested: true,
		MaxAttempts:     1,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := s.Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	applied, err := s.Complete(context.Background(), "y", "w", jobs.Outcome{
		State:   jobs.StateDiscarded,
		Attempt: 1,
		Error:   "boom",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied != jobs.StateDiscarded {
		t.Errorf("applied = %s, want discarded (no override)", applied)
	}
}

func TestUpsertSchedule_PreservesNextRunAtWhenCronUnchanged(t *testing.T) {
	// The contract: re-calling Schedule on every boot with the
	// same cron must not silently advance NextRunAt past missed
	// ticks (which would break CatchUp). Cron changes do update
	// NextRunAt.
	s := memory.New()
	ctx := context.Background()
	originalNext := time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)

	if err := s.UpsertSchedule(ctx, &jobs.ScheduleRow{
		Name:      "daily",
		Kind:      "report",
		Cron:      "0 6 * * *",
		Payload:   []byte(`{"period":"daily"}`),
		NextRunAt: originalNext,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with the SAME cron but a different NextRunAt
	// (mimicking a fresh boot computing parsed.Next(now)).
	laterNext := originalNext.Add(24 * time.Hour)
	if err := s.UpsertSchedule(ctx, &jobs.ScheduleRow{
		Name:      "daily",
		Kind:      "report",
		Cron:      "0 6 * * *",
		Payload:   []byte(`{"period":"daily"}`),
		NextRunAt: laterNext,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rows, _ := s.ListSchedules(ctx)
	if !rows[0].NextRunAt.Equal(originalNext) {
		t.Errorf("NextRunAt after same-cron upsert = %v, want preserved %v",
			rows[0].NextRunAt, originalNext)
	}

	// Re-upsert with a DIFFERENT cron; NextRunAt must update.
	newNext := originalNext.Add(48 * time.Hour)
	if err := s.UpsertSchedule(ctx, &jobs.ScheduleRow{
		Name:      "daily",
		Kind:      "report",
		Cron:      "0 18 * * *",
		Payload:   []byte(`{"period":"daily"}`),
		NextRunAt: newNext,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListSchedules(ctx)
	if !rows[0].NextRunAt.Equal(newNext) {
		t.Errorf("NextRunAt after cron change = %v, want updated to %v",
			rows[0].NextRunAt, newNext)
	}
	if rows[0].Cron != "0 18 * * *" {
		t.Errorf("Cron = %q, want updated", rows[0].Cron)
	}
}

func TestSweepStaleWorkers_RemovesOldRowsOnly(t *testing.T) {
	s := memory.New()
	stale := time.Now().Add(-10 * time.Minute)
	fresh := time.Now()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertWorker(context.Background(), &jobs.WorkerRow{
		ID: "stale", Hostname: "h", Queues: []string{"default"},
		StartedAt: stale, LastSeenAt: stale,
	}))
	must(s.UpsertWorker(context.Background(), &jobs.WorkerRow{
		ID: "alive", Hostname: "h", Queues: []string{"default"},
		StartedAt: fresh, LastSeenAt: fresh,
	}))

	n, err := s.SweepStaleWorkers(context.Background(), time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1", n)
	}

	workers, _ := s.ListWorkers(context.Background())
	if len(workers) != 1 || workers[0].ID != "alive" {
		ids := make([]string, len(workers))
		for i, w := range workers {
			ids[i] = w.ID
		}
		t.Errorf("workers after sweep = %v, want [alive]", ids)
	}
}

func TestGet_DefensiveCopy(t *testing.T) {
	// Mutating the returned row must not affect the store.
	s := memory.New()
	row := &jobs.JobRow{
		ID:        "x",
		Kind:      "k",
		State:     jobs.StateAvailable,
		Error:     "original",
		CreatedAt: time.Now(),
	}
	if err := s.Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(context.Background(), "x")
	got.Error = "mutated"
	again, _ := s.Get(context.Background(), "x")
	if again.Error != "original" {
		t.Errorf("store row mutated through returned pointer: Error=%q", again.Error)
	}
}
