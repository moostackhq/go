package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// --- CancelJob ---

func TestCancelJob_ScheduledTransitionsImmediately(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))
	id := enq(t, m, &trackedJob{Tag: "scheduled"}, jobs.Options{
		Delay: 1 * time.Hour,
	})
	immediate, err := m.CancelJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !immediate {
		t.Error("immediate = false, want true for scheduled job")
	}
	info, _ := m.GetJob(context.Background(), id)
	if info.State != jobs.StateCancelled {
		t.Errorf("State = %s, want cancelled", info.State)
	}
}

func TestCancelJob_AvailableTransitionsImmediately(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))
	id := enq(t, m, &trackedJob{Tag: "avail"}, jobs.Options{})
	immediate, err := m.CancelJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !immediate {
		t.Error("immediate = false, want true for available job")
	}
	info, _ := m.GetJob(context.Background(), id)
	if info.State != jobs.StateCancelled {
		t.Errorf("State = %s, want cancelled", info.State)
	}
}

func TestCancelJob_RunningTerminatesWithinHeartbeat(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{}) // never closes
	observed := make(chan error, 1)
	ctl := &trackedJobControl{gate: gate, observedCtxDone: observed}
	registerTrackedJob("running", ctl)
	id := enq(t, m, &trackedJob{Tag: "running"}, jobs.Options{
		MaxAttempts: 1, // do not retry cancellation
	})

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     300 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     50 * time.Millisecond,
	})
	done := startWorker(t, w)
	defer func() {
		close(gate)
		w.Stop(context.Background())
		<-done
	}()

	// Wait for the job to start.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && ctl.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if ctl.runs.Load() == 0 {
		t.Fatal("job did not start")
	}

	immediate, err := m.CancelJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if immediate {
		t.Error("immediate = true, want false for running job (cancellation is deferred)")
	}

	// Heartbeat interval is 50ms; the job should observe cancellation
	// within ~one heartbeat. Allow 250ms for scheduling slack.
	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("observed err = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("job did not observe cancellation within heartbeat window")
	}

	waitForState(t, m, id, jobs.StateCancelled, 1*time.Second)

	attempts := waitForAttempts(t, m, id, 1, 1*time.Second)
	if attempts[0].State != jobs.AttemptCancelled {
		t.Errorf("attempt state = %q, want cancelled", attempts[0].State)
	}
}

func TestCancelJob_RaceWithSuccess_LandsAsCancelled(t *testing.T) {
	// The cancel-then-success race: the heartbeat goroutine never
	// observes cancel_requested because Run finishes first. The
	// store.Complete override must still rewrite the success to
	// cancelled so the user's CancelJob is not silently lost.
	//
	// Deterministic setup: HeartbeatInterval = 10s (won't tick
	// during the test). The worker claims, starts Run; the test
	// calls CancelJob (sets the flag on the row); the gate is
	// opened so Run completes successfully; the runner submits
	// StateSucceeded; the store overrides to StateCancelled.
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{})
	ctl := &trackedJobControl{gate: gate}
	registerTrackedJob("race", ctl)
	id := enq(t, m, &trackedJob{Tag: "race"}, jobs.Options{MaxAttempts: 1})

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     60 * time.Second, // 3x heartbeat = 30s, so plenty of headroom
		HeartbeatInterval: 10 * time.Second, // won't tick during this test
		SweepInterval:     30 * time.Second,
	})
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	// Wait for the job to enter Run.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && ctl.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if ctl.runs.Load() == 0 {
		t.Fatal("job did not start")
	}

	// Cancel, then immediately let Run succeed.
	if _, err := m.CancelJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	close(gate)

	waitForState(t, m, id, jobs.StateCancelled, 1*time.Second)

	// The attempt ledger must mirror the override: AttemptCancelled,
	// not AttemptSucceeded.
	attempts := waitForAttempts(t, m, id, 1, 1*time.Second)
	if attempts[0].State != jobs.AttemptCancelled {
		t.Errorf("attempt state = %q, want cancelled (mirrors the store override)", attempts[0].State)
	}
}

func TestCancelJob_TerminalErrors(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("done", &trackedJobControl{})
	id := enq(t, m, &trackedJob{Tag: "done"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()
	waitForState(t, m, id, jobs.StateSucceeded, 1*time.Second)

	_, err := m.CancelJob(context.Background(), id)
	if !errors.Is(err, jobs.ErrJobTerminal) {
		t.Errorf("want ErrJobTerminal, got %v", err)
	}
}

// --- RetryJob ---

func TestRetryJob_RevivesFailedJob(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	// First, push a job to discarded by exhausting attempts.
	ctl := &trackedJobControl{returnErr: errors.New("nope")}
	registerTrackedJob("revive", ctl)
	id := enq(t, m, &trackedJob{Tag: "revive"}, jobs.Options{MaxAttempts: 1})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	waitForState(t, m, id, jobs.StateDiscarded, 1*time.Second)
	w.Stop(context.Background())
	<-done

	// Now succeed on the retry and Retry the job.
	ctl.returnErr = nil
	if err := m.RetryJob(context.Background(), id); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	info, _ := m.GetJob(context.Background(), id)
	if info.State != jobs.StateAvailable {
		t.Errorf("state after Retry = %s, want available", info.State)
	}

	w2, _ := jobs.NewWorker(m, fastWorkerConfig())
	done2 := startWorker(t, w2)
	defer func() {
		w2.Stop(context.Background())
		<-done2
	}()
	waitForState(t, m, id, jobs.StateSucceeded, 1*time.Second)

	// Attempt count should reflect both rounds.
	attempts := waitForAttempts(t, m, id, 2, 1*time.Second)
	if len(attempts) != 2 {
		t.Errorf("attempts after retry = %d, want 2", len(attempts))
	}
}

func TestRetryJob_OnRunningErrors(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("running", &trackedJobControl{})
	id := enq(t, m, &trackedJob{Tag: "running"}, jobs.Options{})

	err := m.RetryJob(context.Background(), id)
	if !errors.Is(err, jobs.ErrJobNotRetryable) {
		t.Errorf("want ErrJobNotRetryable, got %v", err)
	}
}

// --- DeleteJob ---

func TestDeleteJob_RefusesRunningJob(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{})
	ctl := &trackedJobControl{gate: gate}
	registerTrackedJob("hold", ctl)
	id := enq(t, m, &trackedJob{Tag: "hold"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		close(gate)
		w.Stop(context.Background())
		<-done
	}()

	// Wait for it to start.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && ctl.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if ctl.runs.Load() == 0 {
		t.Fatal("job did not start")
	}

	err := m.DeleteJob(context.Background(), id)
	if !errors.Is(err, jobs.ErrJobRunning) {
		t.Errorf("want ErrJobRunning, got %v", err)
	}
}

func TestDeleteJob_RemovesTerminalJob(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))
	registerTrackedJob("delete", &trackedJobControl{})
	id := enq(t, m, &trackedJob{Tag: "delete"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	waitForState(t, m, id, jobs.StateSucceeded, 1*time.Second)
	w.Stop(context.Background())
	<-done

	if err := m.DeleteJob(context.Background(), id); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	_, err := m.GetJob(context.Background(), id)
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("after delete, GetJob err = %v, want ErrNotFound", err)
	}
}

// --- ListWorkers / ListQueues ---

func TestListWorkers_ShowsLiveWorkers(t *testing.T) {
	m, _ := jobs.New(memory.New(), jobs.Config{})
	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	// Give Start a tick to call UpsertWorker.
	time.Sleep(50 * time.Millisecond)

	workers, err := m.ListWorkers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(workers))
	}
	if workers[0].ID != w.ID() {
		t.Errorf("worker id = %q, want %q", workers[0].ID, w.ID())
	}
}

func TestListWorkers_SweepRemovesCrashedRow(t *testing.T) {
	// A crashed worker's row sits in the table indefinitely
	// because Stop never ran. The sweep loop should remove it
	// once LastSeenAt is older than staleWorkerMultiplier *
	// LeaseDuration.
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})

	// Pretend a worker registered itself 10 minutes ago and died.
	ago := time.Now().Add(-10 * time.Minute)
	if err := store.UpsertWorker(context.Background(), &jobs.WorkerRow{
		ID: "crashed-ghost", Hostname: "h", Queues: []string{"default"},
		StartedAt: ago, LastSeenAt: ago,
	}); err != nil {
		t.Fatal(err)
	}

	// Live worker with tight intervals: LeaseDuration = 200ms means
	// the stale threshold is 1s, well below the ghost's 10 min.
	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     200 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		SweepInterval:     20 * time.Millisecond,
	})
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		workers, _ := m.ListWorkers(context.Background())
		if len(workers) == 1 && workers[0].ID == w.ID() {
			return // ghost reaped, only the live worker remains
		}
		time.Sleep(10 * time.Millisecond)
	}
	workers, _ := m.ListWorkers(context.Background())
	ids := make([]string, len(workers))
	for i, w := range workers {
		ids[i] = w.ID
	}
	t.Errorf("workers = %v, want just %s (ghost should have been reaped)", ids, w.ID())
}

func TestListWorkers_RemovedOnStop(t *testing.T) {
	m, _ := jobs.New(memory.New(), jobs.Config{})
	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	time.Sleep(50 * time.Millisecond)
	w.Stop(context.Background())
	<-done

	workers, _ := m.ListWorkers(context.Background())
	if len(workers) != 0 {
		t.Errorf("workers after Stop = %d, want 0", len(workers))
	}
}

func TestListQueues_PerStateCounts(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))

	// 3 available on "default", 2 scheduled on "emails", 1 cancelled on "reports".
	for i := 0; i < 3; i++ {
		enq(t, m, &trackedJob{Tag: "default"}, jobs.Options{})
	}
	for i := 0; i < 2; i++ {
		enq(t, m, &trackedJob{Tag: "emails"}, jobs.Options{
			Queue: "emails",
			Delay: 1 * time.Hour,
		})
	}
	id := enq(t, m, &trackedJob{Tag: "reports"}, jobs.Options{Queue: "reports"})
	_, _ = m.CancelJob(context.Background(), id)

	queues, err := m.ListQueues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]jobs.QueueInfo{}
	for _, q := range queues {
		byName[q.Name] = q
	}
	if got := byName["default"].Counts[jobs.StateAvailable]; got != 3 {
		t.Errorf("default available = %d, want 3", got)
	}
	if got := byName["emails"].Counts[jobs.StateScheduled]; got != 2 {
		t.Errorf("emails scheduled = %d, want 2", got)
	}
	if got := byName["reports"].Counts[jobs.StateCancelled]; got != 1 {
		t.Errorf("reports cancelled = %d, want 1", got)
	}
}
