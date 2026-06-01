package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// --- shared test harness ---
//
// trackedJob is a test job type whose Run behaviour is driven by a
// per-Tag entry in `trackedJobs`. Tests populate the registry before
// enqueueing, then read counters after the worker drains.
type trackedJob struct {
	Tag string `json:"tag"`
}

type trackedJobControl struct {
	runs      atomic.Int32
	returnErr error
	panicMsg  string
	// gate, if non-nil, blocks Run until closed. Used to hold the
	// job in-flight while tests probe state.
	gate chan struct{}
	// observedCtxDone, if non-nil, gets sent the value of ctx.Err()
	// when ctx.Done() fires inside Run. Used for cancellation tests.
	observedCtxDone chan error
	// blockUntilCtxOnFirstN: on the first N attempts, Run blocks
	// until ctx is cancelled (used to trigger deterministic
	// per-attempt timeouts). Later attempts run normally.
	blockUntilCtxOnFirstN int32
	// failFirstN: on the first N attempts, Run returns a transient
	// error so the job is retried; later attempts succeed. Used to
	// test retry / backoff behaviour without racing on returnErr.
	failFirstN int32
}

var (
	trackedJobsMu sync.Mutex
	trackedJobs   = map[string]*trackedJobControl{}
)

func resetTrackedJobs() {
	trackedJobsMu.Lock()
	trackedJobs = map[string]*trackedJobControl{}
	trackedJobsMu.Unlock()
}

func registerTrackedJob(tag string, c *trackedJobControl) {
	trackedJobsMu.Lock()
	trackedJobs[tag] = c
	trackedJobsMu.Unlock()
}

func (j *trackedJob) Run(ctx jobs.Context) error {
	trackedJobsMu.Lock()
	c := trackedJobs[j.Tag]
	trackedJobsMu.Unlock()
	if c == nil {
		return fmt.Errorf("no control registered for tag %q", j.Tag)
	}
	c.runs.Add(1)
	if c.panicMsg != "" {
		panic(c.panicMsg)
	}
	if c.runs.Load() <= c.blockUntilCtxOnFirstN {
		<-ctx.Done()
		if c.observedCtxDone != nil {
			c.observedCtxDone <- ctx.Err()
		}
		return ctx.Err()
	}
	if c.runs.Load() <= c.failFirstN {
		return errors.New("transient")
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			if c.observedCtxDone != nil {
				c.observedCtxDone <- ctx.Err()
			}
			return ctx.Err()
		}
	}
	return c.returnErr
}

// waitForState polls GetJob until the job reaches want or timeout.
func waitForState(t *testing.T, m *jobs.Manager, id string, want jobs.State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := m.GetJob(context.Background(), id)
		if err == nil && info.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	info, _ := m.GetJob(context.Background(), id)
	t.Fatalf("timeout waiting for state=%s; last=%+v", want, info)
}

// waitForAttempts polls ListJobAttempts until want rows exist or
// timeout. The runner writes the attempt row AFTER Complete (so it
// can mirror any store-side state override), so callers reading
// the ledger right after a state transition may see it lag by one
// store write. Use this helper instead of asserting len(attempts)
// directly from waitForState.
func waitForAttempts(t *testing.T, m *jobs.Manager, id string, want int, timeout time.Duration) []jobs.Attempt {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var attempts []jobs.Attempt
	for time.Now().Before(deadline) {
		page, _ := m.ListJobAttempts(context.Background(), id, jobs.AttemptsFilter{})
		attempts = page.Attempts
		if len(attempts) >= want {
			return attempts
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d attempts; got %d", want, len(attempts))
	return attempts
}

// startWorker spawns w.Start in a goroutine and returns a done channel.
func startWorker(t *testing.T, w *jobs.Worker) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Start(context.Background())
	}()
	return done
}

func fastWorkerConfig() jobs.WorkerConfig {
	return jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     20 * time.Millisecond,
	}
}

// --- NewWorker ---

func TestNewWorker_NilManager(t *testing.T) {
	if _, err := jobs.NewWorker(nil, jobs.WorkerConfig{}); err == nil {
		t.Error("expected error for nil manager")
	}
}

func TestNewWorker_LeaseHeartbeatRatio(t *testing.T) {
	m := newManager(t)
	_, err := jobs.NewWorker(m, jobs.WorkerConfig{
		LeaseDuration:     50 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond, // 1x, must be at least 3x
	})
	if err == nil {
		t.Error("expected error for lease/heartbeat ratio < 3")
	}
}

func TestNewWorker_DefaultsApplied(t *testing.T) {
	m := newManager(t)
	w, err := jobs.NewWorker(m, jobs.WorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID() == "" {
		t.Error("default ID was empty")
	}
}

// --- Run loop ---

func TestWorker_RunsJobToCompletion(t *testing.T) {
	resetTrackedJobs()
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))

	ctl := &trackedJobControl{}
	registerTrackedJob("ok", ctl)

	id := enq(t, m, &trackedJob{Tag: "ok"}, jobs.Options{})

	w, err := jobs.NewWorker(m, fastWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateSucceeded, 2*time.Second)
	if r := ctl.runs.Load(); r != 1 {
		t.Errorf("runs = %d, want 1", r)
	}
}

func TestWorker_RetriesOnError_DiscardsAfterMaxAttempts(t *testing.T) {
	resetTrackedJobs()
	// Fast backoff so the test does not wait the 1s+2s default.
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond, Max: 5 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	ctl := &trackedJobControl{returnErr: errors.New("boom")}
	registerTrackedJob("err", ctl)

	id := enq(t, m, &trackedJob{Tag: "err"}, jobs.Options{
		MaxAttempts: 3,
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateDiscarded, 3*time.Second)
	if r := ctl.runs.Load(); r != 3 {
		t.Errorf("runs = %d, want 3 (one per attempt)", r)
	}
	info, _ := m.GetJob(context.Background(), id)
	if info.Error == "" {
		t.Error("Error message should be persisted on discard")
	}
	if info.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", info.Attempt)
	}
}

func TestWorker_PanicRecoveredAsError(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond, Max: 2 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	ctl := &trackedJobControl{panicMsg: "kaboom"}
	registerTrackedJob("panic", ctl)

	id := enq(t, m, &trackedJob{Tag: "panic"}, jobs.Options{
		MaxAttempts: 2,
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateDiscarded, 2*time.Second)
	info, _ := m.GetJob(context.Background(), id)
	if info.Error == "" || info.Error[:6] != "panic:" {
		t.Errorf("Error = %q, want 'panic: kaboom'", info.Error)
	}
}

func TestWorker_UnregisteredKind_ParkedAsFailed(t *testing.T) {
	// Pre-insert a row with a kind the manager will not know about.
	m := newManager(t)
	// Build the worker first, but do NOT register the kind.
	store := memory.New()
	m, _ = jobs.New(store, jobs.Config{})

	// Directly insert a row to simulate "older binary wrote this."
	row := &jobs.JobRow{
		ID:          "ghost-1",
		Kind:        "vanished",
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
	info, _ := m.GetJob(context.Background(), "ghost-1")
	if info.Error == "" {
		t.Error("expected error message for unregistered kind")
	}
}

// --- Stop / drain ---

func TestWorker_StopWaitsForInFlight(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{})
	ctl := &trackedJobControl{gate: gate}
	registerTrackedJob("gated", ctl)

	id := enq(t, m, &trackedJob{Tag: "gated"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)

	// Wait for the job to start.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && ctl.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if ctl.runs.Load() == 0 {
		t.Fatal("job did not start")
	}

	// Stop with no deadline; this should block until we release the gate.
	stopDone := make(chan struct{})
	go func() {
		w.Stop(context.Background())
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop returned before in-flight job finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate) // let the job finish
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after job finished")
	}
	<-done
	waitForState(t, m, id, jobs.StateSucceeded, 500*time.Millisecond)
}

func TestWorker_StopWithDeadlineCancelsInFlight(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{}) // never closed
	observed := make(chan error, 1)
	ctl := &trackedJobControl{gate: gate, observedCtxDone: observed}
	registerTrackedJob("hang", ctl)

	enq(t, m, &trackedJob{Tag: "hang"}, jobs.Options{
		MaxAttempts: 1, // do not retry the cancellation
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && ctl.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if ctl.runs.Load() == 0 {
		t.Fatal("job did not start")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w.Stop(stopCtx)

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ctx.Err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("job did not observe cancellation")
	}
	<-done
}

func TestWorker_StartExitsOnCtxCancel(t *testing.T) {
	m := newManager(t)
	w, _ := jobs.NewWorker(m, fastWorkerConfig())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Start(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start did not return after ctx cancel")
	}
}

// --- Sweep / crash recovery ---

func TestWorker_PerJobBackoffOverridesManagerDefault(t *testing.T) {
	// Manager default is unreachably slow; per-job backoff is fast.
	// If the override survives the enqueue → claim → fail → retry
	// cycle, the second attempt fires within ~200ms. If only the
	// manager default were honoured (the old bug), the retry would
	// take 100s and the test would time out.
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 100 * time.Second, Max: 100 * time.Second},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	// First attempt errors, second succeeds. Deterministic via
	// the failFirstN counter; no goroutine-driven mutation.
	ctl := &trackedJobControl{failFirstN: 1}
	registerTrackedJob("backoff-override", ctl)
	id := enq(t, m, &trackedJob{Tag: "backoff-override"}, jobs.Options{
		MaxAttempts: 2,
		Backoff:     jobs.ExponentialBackoff{Base: 1 * time.Millisecond, Max: 5 * time.Millisecond},
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	// 1s is far below the 100s manager-default backoff, so reaching
	// StateSucceeded within this budget proves the per-job override
	// drove the retry.
	waitForState(t, m, id, jobs.StateSucceeded, 1*time.Second)
	if ctl.runs.Load() != 2 {
		t.Errorf("runs = %d, want 2 (one fail + one success)", ctl.runs.Load())
	}
}

func TestWorker_SweepWritesSyntheticAttempt(t *testing.T) {
	// Crashed worker leaves a lease behind; SweepExpired should
	// reclaim AND write a 'failed' Attempt row attributed to the
	// crashed worker, so the ledger doesn't show a gap.
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("orphan", &trackedJobControl{})
	id := enq(t, m, &trackedJob{Tag: "orphan"}, jobs.Options{})

	now := time.Now().UTC()
	claimed, err := store.Claim(context.Background(), jobs.ClaimRequest{
		WorkerID:      "crashed",
		Queues:        []string{"default"},
		Now:           now,
		LeaseDuration: 20 * time.Millisecond,
		Limit:         1,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("setup claim: claimed=%v err=%v", claimed, err)
	}
	time.Sleep(40 * time.Millisecond) // let the lease expire

	n, err := store.SweepExpired(context.Background(), time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("SweepExpired: n=%d err=%v", n, err)
	}

	page, err := m.ListJobAttempts(context.Background(), id, jobs.AttemptsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	attempts := page.Attempts
	if len(attempts) != 1 {
		t.Fatalf("attempts after sweep = %d, want 1", len(attempts))
	}
	a := attempts[0]
	if a.State != jobs.AttemptFailed || a.Error != "lease expired" {
		t.Errorf("synthetic attempt = %+v, want failed/lease expired", a)
	}
	if a.WorkerID != "crashed" {
		t.Errorf("synthetic attempt WorkerID = %q, want crashed", a.WorkerID)
	}
	if a.Attempt != 1 {
		t.Errorf("synthetic attempt number = %d, want 1", a.Attempt)
	}

	info, _ := m.GetJob(context.Background(), id)
	if info.Attempt != 1 {
		t.Errorf("row Attempt after sweep = %d, want 1 (bumped to count the crashed run)", info.Attempt)
	}
}

func TestWorker_SweepLabelsAttemptCancelledWhenCancelRequested(t *testing.T) {
	// The user calls CancelJob on a running job; the worker crashes
	// before the heartbeat observes the flag; SweepExpired reclaims.
	// The synthetic attempt should be labelled cancelled (not the
	// generic "lease expired" failure) so the ledger reflects the
	// user's intent.
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("crash-cancel", &trackedJobControl{})
	id := enq(t, m, &trackedJob{Tag: "crash-cancel"}, jobs.Options{})

	// Pretend a worker claimed it with a tiny lease, then died.
	now := time.Now().UTC()
	claimed, err := store.Claim(context.Background(), jobs.ClaimRequest{
		WorkerID:      "crashed-worker",
		Queues:        []string{"default"},
		Now:           now,
		LeaseDuration: 20 * time.Millisecond,
		Limit:         1,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("setup claim failed: claimed=%v err=%v", claimed, err)
	}

	// User cancels while the (hypothetically alive) worker is
	// running. Returns immediate=false because state is running.
	immediate, err := m.CancelJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if immediate {
		t.Fatal("immediate = true, want false (running job)")
	}

	// Let the lease expire, then sweep.
	time.Sleep(40 * time.Millisecond)
	n, err := store.SweepExpired(context.Background(), time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("SweepExpired: n=%d err=%v", n, err)
	}

	attempts := waitForAttempts(t, m, id, 1, 1*time.Second)
	a := attempts[0]
	if a.State != jobs.AttemptCancelled {
		t.Errorf("synthetic attempt state = %q, want cancelled (cancel was pending at crash)", a.State)
	}
	if a.Error != "lease expired during cancellation" {
		t.Errorf("synthetic attempt error = %q, want %q", a.Error, "lease expired during cancellation")
	}
}

func TestWorker_SweepReclaimsExpiredLease(t *testing.T) {
	// Simulate a crashed worker by directly claiming a job from the
	// store and never completing it. After the lease expires, a
	// running worker's sweep should return it to available; the next
	// claim should pick it up and finish normally.
	resetTrackedJobs()
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	ctl := &trackedJobControl{}
	registerTrackedJob("recover", ctl)
	id := enq(t, m, &trackedJob{Tag: "recover"}, jobs.Options{})

	// Pretend a worker named "crashed" claimed it with a 50ms lease,
	// then died (we never call Complete).
	crashed := time.Now().UTC()
	claimed, err := store.Claim(context.Background(), jobs.ClaimRequest{
		WorkerID:      "crashed-worker",
		Queues:        []string{"default"},
		Now:           crashed,
		LeaseDuration: 50 * time.Millisecond,
		Limit:         1,
	})
	if err != nil || len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("setup claim failed: claimed=%v err=%v", claimed, err)
	}

	// Spin up a real worker. Sweep should reclaim once the 50ms lease expires.
	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     10 * time.Millisecond,
	})
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateSucceeded, 2*time.Second)
	if ctl.runs.Load() != 1 {
		t.Errorf("runs = %d, want 1 (only the recovery attempt)", ctl.runs.Load())
	}
}

// --- Goroutine leak guard ---

func TestWorker_StopReleasesAllGoroutines(t *testing.T) {
	// A coarse check that Stop returning means everything we spawned
	// has wound down. We compare goroutine counts before and after
	// with a small allowance for runtime noise.
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	for i := 0; i < 5; i++ {
		tag := fmt.Sprintf("g%d", i)
		registerTrackedJob(tag, &trackedJobControl{})
		enq(t, m, &trackedJob{Tag: tag}, jobs.Options{})
	}

	before := goroutineCount()

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		Concurrency:       3,
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     20 * time.Millisecond,
	})
	done := startWorker(t, w)

	// Wait for all jobs to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, _ := m.ListJobs(context.Background(), jobs.JobFilter{
			States: []jobs.State{jobs.StateSucceeded},
		})
		if len(page.Jobs) == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	w.Stop(context.Background())
	<-done

	// Give the scheduler a moment to reap helper goroutines.
	time.Sleep(50 * time.Millisecond)
	after := goroutineCount()
	if after > before+2 {
		t.Errorf("goroutine count grew from %d to %d after Stop", before, after)
	}
}

func goroutineCount() int {
	return runtime.NumGoroutine()
}
