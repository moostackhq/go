package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// hookRecorder collects every hook call so tests can assert
// ordering, counts, and arguments.
type hookRecorder struct {
	mu       sync.Mutex
	enqueues []hookEnqueueEvent
	starts   []hookStartEvent
	finishes []hookFinishEvent
}

type hookEnqueueEvent struct {
	jobID string
	kind  string
	opts  jobs.Options
	tx    bool
}

type hookStartEvent struct {
	jobID   string
	kind    string
	attempt int
}

type hookFinishEvent struct {
	jobID   string
	attempt int
	err     error
	dur     time.Duration
}

func (r *hookRecorder) hooks() jobs.Hooks {
	return jobs.Hooks{
		OnEnqueue: func(_ context.Context, e jobs.EnqueueEvent) {
			r.mu.Lock()
			r.enqueues = append(r.enqueues, hookEnqueueEvent{
				jobID: e.JobID,
				kind:  e.Kind,
				opts:  e.Opts,
				tx:    e.Tx,
			})
			r.mu.Unlock()
		},
		OnStart: func(_ context.Context, e jobs.StartEvent) {
			r.mu.Lock()
			r.starts = append(r.starts, hookStartEvent{
				jobID:   e.JobID,
				kind:    e.Kind,
				attempt: e.Attempt,
			})
			r.mu.Unlock()
		},
		OnFinish: func(_ context.Context, e jobs.FinishEvent) {
			r.mu.Lock()
			r.finishes = append(r.finishes, hookFinishEvent{
				jobID:   e.JobID,
				attempt: e.Attempt,
				err:     e.Err,
				dur:     e.Dur,
			})
			r.mu.Unlock()
		},
	}
}

func TestHooks_FireInOrderForSuccess(t *testing.T) {
	resetTrackedJobs()
	rec := &hookRecorder{}
	m, _ := jobs.New(memory.New(), jobs.Config{Hooks: rec.hooks()})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("ok", &trackedJobControl{})

	id := enq(t, m, &trackedJob{Tag: "ok"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()
	waitForState(t, m, id, jobs.StateSucceeded, 1*time.Second)

	rec.mu.Lock()
	defer rec.mu.Unlock()

	if len(rec.enqueues) != 1 {
		t.Fatalf("enqueues = %d, want 1", len(rec.enqueues))
	}
	if len(rec.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(rec.starts))
	}
	if len(rec.finishes) != 1 {
		t.Fatalf("finishes = %d, want 1", len(rec.finishes))
	}
	if rec.starts[0].jobID != id || rec.starts[0].kind != "tracked" || rec.starts[0].attempt != 1 {
		t.Errorf("start = %+v", rec.starts[0])
	}
	if rec.finishes[0].err != nil {
		t.Errorf("finish err = %v, want nil", rec.finishes[0].err)
	}
	if rec.finishes[0].dur <= 0 {
		t.Errorf("finish dur = %v, want >0", rec.finishes[0].dur)
	}
}

func TestHooks_OnFinishCapturesError(t *testing.T) {
	resetTrackedJobs()
	rec := &hookRecorder{}
	m, _ := jobs.New(memory.New(), jobs.Config{
		Hooks:          rec.hooks(),
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("err", &trackedJobControl{returnErr: errors.New("boom")})

	enq(t, m, &trackedJob{Tag: "err"}, jobs.Options{MaxAttempts: 2})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	// Wait for both attempts to finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.finishes)
		rec.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.finishes) != 2 {
		t.Fatalf("finishes = %d, want 2", len(rec.finishes))
	}
	for i, f := range rec.finishes {
		if f.err == nil {
			t.Errorf("finishes[%d].err = nil, want non-nil", i)
		}
		if f.attempt != i+1 {
			t.Errorf("finishes[%d].attempt = %d, want %d", i, f.attempt, i+1)
		}
	}
}

func TestHooks_OnFinishCapturesPanic(t *testing.T) {
	resetTrackedJobs()
	rec := &hookRecorder{}
	m, _ := jobs.New(memory.New(), jobs.Config{
		Hooks:          rec.hooks(),
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("panic", &trackedJobControl{panicMsg: "kaboom"})

	enq(t, m, &trackedJob{Tag: "panic"}, jobs.Options{MaxAttempts: 1})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.finishes)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.finishes) != 1 {
		t.Fatalf("finishes = %d, want 1", len(rec.finishes))
	}
	if rec.finishes[0].err == nil {
		t.Error("expected non-nil err from panic")
	}
}

func TestHooks_PanicInOnEnqueueRecovered(t *testing.T) {
	// A panicking OnEnqueue must not propagate up to the caller's
	// Enqueue call (would crash the user's HTTP handler).
	m, _ := jobs.New(memory.New(), jobs.Config{
		Hooks: jobs.Hooks{
			OnEnqueue: func(_ context.Context, _ jobs.EnqueueEvent) {
				panic("oops")
			},
		},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	// Should not panic, should not error.
	id, err := m.Enqueue(context.Background(), &trackedJob{Tag: "boom"})
	if err != nil {
		t.Fatalf("Enqueue returned error after panicking hook: %v", err)
	}
	if id == "" {
		t.Error("Enqueue returned empty id after panicking hook")
	}
}

func TestHooks_PanicInOnStartAndOnFinishRecovered(t *testing.T) {
	// Both hooks panic. The worker must drain normally and the job
	// must reach a terminal state.
	resetTrackedJobs()
	registerTrackedJob("p", &trackedJobControl{})
	m, _ := jobs.New(memory.New(), jobs.Config{
		Hooks: jobs.Hooks{
			OnStart:  func(_ context.Context, _ jobs.StartEvent) { panic("start") },
			OnFinish: func(_ context.Context, _ jobs.FinishEvent) { panic("finish") },
		},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	id := enq(t, m, &trackedJob{Tag: "p"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()
	waitForState(t, m, id, jobs.StateSucceeded, 1*time.Second)
}

func TestHooks_FireForUnregisteredKindPath(t *testing.T) {
	// A worker that pulls a row whose Kind has no registration
	// parks it as failed without ever invoking Run. Observability
	// hooks should still fire (with a synthetic event) so metrics
	// counting "attempts started/finished per kind" don't miss
	// these failures.
	rec := &hookRecorder{}
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{Hooks: rec.hooks()})

	// Pre-insert a row directly; no Register call.
	row := &jobs.JobRow{
		ID:          "ghost-1",
		Kind:        "ghost",
		Payload:     []byte("{}"),
		Queue:       "default",
		State:       jobs.StateAvailable,
		MaxAttempts: 3,
		AvailableAt: time.Now().UTC(),
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := store.Insert(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()
	waitForState(t, m, "ghost-1", jobs.StateFailed, 1*time.Second)

	// Give OnFinish (deferred) a moment to run.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		nFinish := len(rec.finishes)
		rec.mu.Unlock()
		if nFinish >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.starts) != 1 {
		t.Errorf("OnStart fired %d times, want 1", len(rec.starts))
	}
	if len(rec.finishes) != 1 {
		t.Fatalf("OnFinish fired %d times, want 1", len(rec.finishes))
	}
	f := rec.finishes[0]
	if f.jobID != "ghost-1" {
		t.Errorf("finish JobID = %q, want ghost-1", f.jobID)
	}
	if f.err == nil {
		t.Error("finish Err = nil, want unregistered-kind error")
	}
	// The error wraps ErrUnregistered so callers can match.
	if !errors.Is(f.err, jobs.ErrUnregistered) {
		t.Errorf("finish Err = %v, want errors.Is(.., ErrUnregistered)", f.err)
	}
}

func TestHooks_OnEnqueueOptsBackoffIsEffective(t *testing.T) {
	// OnEnqueue should see the EFFECTIVE backoff (manager default
	// when user didn't override), so hook authors can log/meter
	// "what schedule will this job use on retry?". But the row
	// itself must NOT have the default baked into BackoffSpec —
	// the runner already falls back to manager default at retry
	// time, and persisting the default would pin it to whatever
	// the manager had at enqueue time.
	rec := &hookRecorder{}
	managerDefault := jobs.ExponentialBackoff{Base: 42 * time.Second, Max: 1 * time.Hour, Jitter: 0.1}
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{
		Hooks:          rec.hooks(),
		DefaultBackoff: managerDefault,
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	// User does NOT set Options.Backoff — falls back to default.
	id := enq(t, m, &trackedJob{Tag: "default-backoff"}, jobs.Options{})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.enqueues) != 1 {
		t.Fatalf("enqueues = %d, want 1", len(rec.enqueues))
	}
	got := rec.enqueues[0].opts.Backoff
	if got == nil {
		t.Fatal("OnEnqueue saw Opts.Backoff = nil, want manager default")
	}
	if got != managerDefault {
		t.Errorf("OnEnqueue saw Opts.Backoff = %v, want manager default %v", got, managerDefault)
	}

	// Persistence: row.BackoffSpec must be empty so the manager
	// default isn't pinned to the row.
	row, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.BackoffSpec) != 0 {
		t.Errorf("row.BackoffSpec = %q, want empty (manager default should not be persisted)", row.BackoffSpec)
	}
}

func TestHooks_OnEnqueueCarriesJobIDAndKind(t *testing.T) {
	rec := &hookRecorder{}
	m, _ := jobs.New(memory.New(), jobs.Config{Hooks: rec.hooks()})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	id := enq(t, m, &trackedJob{Tag: "no-worker"}, jobs.Options{Queue: "Q"})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.enqueues) != 1 {
		t.Fatalf("enqueues = %d, want 1", len(rec.enqueues))
	}
	e := rec.enqueues[0]
	if e.jobID != id {
		t.Errorf("event JobID = %q, want %q", e.jobID, id)
	}
	if e.kind != "tracked" {
		t.Errorf("event Kind = %q, want tracked", e.kind)
	}
	if e.opts.Queue != "Q" {
		t.Errorf("opts.Queue = %q, want Q", e.opts.Queue)
	}
	if e.tx {
		t.Error("event Tx = true, want false for non-Tx enqueue")
	}
}

func TestHooks_OnEnqueueDoesNotFireOnDuplicate(t *testing.T) {
	// A duplicate UniqueKey collision returns an error and does not
	// produce a new row. OnEnqueue should not fire for the rejected
	// call: observers that count enqueues would otherwise overcount.
	rec := &hookRecorder{}
	m, _ := jobs.New(memory.New(), jobs.Config{Hooks: rec.hooks()})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	if _, err := m.Enqueue(context.Background(),
		&trackedJob{Tag: "first"}, jobs.Options{UniqueKey: "u1"}); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	// Second enqueue with the same key: returns DuplicateError, no
	// new row, no hook fire.
	_, err := m.Enqueue(context.Background(),
		&trackedJob{Tag: "second"}, jobs.Options{UniqueKey: "u1"})
	if !errors.Is(err, jobs.ErrDuplicate) {
		t.Fatalf("second Enqueue: err = %v, want ErrDuplicate", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.enqueues) != 1 {
		t.Errorf("enqueues = %d, want 1 (duplicate must not fire OnEnqueue)", len(rec.enqueues))
	}
}
