package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerConfig controls a single worker's runtime behaviour. The
// zero value is usable: defaults are filled in by [NewWorker].
type WorkerConfig struct {
	// ID identifies this worker in the locked_by column and in
	// [Manager.ListWorkers] output. Generated when empty.
	ID string
	// Queues this worker pulls from. Defaults to ["default"].
	Queues []string
	// Concurrency caps the number of jobs running on this worker at
	// any moment. Defaults to 1.
	Concurrency int
	// PerQueue caps in-flight jobs per queue on this worker.
	// Unset keys mean no per-queue cap.
	PerQueue map[string]int
	// PollInterval is the cadence at which the worker checks for
	// new jobs. Defaults to 1s.
	PollInterval time.Duration
	// LeaseDuration is how long a claim is valid before another
	// worker can sweep it back to available. Defaults to 60s.
	LeaseDuration time.Duration
	// HeartbeatInterval is how often a per-job goroutine extends
	// the lease and checks for a cancellation request. Defaults to
	// LeaseDuration/3 (so two heartbeats can be lost before the
	// lease expires).
	HeartbeatInterval time.Duration
	// SweepInterval is the cadence at which the worker reclaims
	// jobs whose leases have expired (typically because the
	// previous owner crashed). Defaults to 30s.
	SweepInterval time.Duration
}

// Worker drives the run loop: claim eligible jobs, dispatch each to
// a goroutine, heartbeat the lease, and persist the outcome.
type Worker struct {
	manager *Manager
	cfg     WorkerConfig

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}

	// drainCtx is set by Stop; Start reads it during shutdown.
	drainMu  sync.Mutex
	drainCtx context.Context

	// In-flight tracking. sem caps concurrency; wg tracks running
	// goroutines; cancels maps jobID to the per-job CancelFunc so
	// Stop can force-cancel on drain timeout. queueInFlight is the
	// per-queue counter the PerQueue cap reads.
	sem           chan struct{}
	wg            sync.WaitGroup
	cancels       sync.Map // map[string]context.CancelFunc
	queueInFlight sync.Map // map[string]*atomic.Int32
}

// NewWorker constructs a worker bound to the given manager. Fills
// in default values for any zero-valued [WorkerConfig] fields and
// validates the lease/heartbeat relationship.
func NewWorker(m *Manager, cfg WorkerConfig) (*Worker, error) {
	if m == nil {
		return nil, fmt.Errorf("jobs.NewWorker: nil manager")
	}
	if cfg.ID == "" {
		cfg.ID = workerID()
	}
	if len(cfg.Queues) == 0 {
		cfg.Queues = []string{m.config.DefaultQueue}
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 60 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = cfg.LeaseDuration / 3
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if cfg.LeaseDuration < 3*cfg.HeartbeatInterval {
		return nil, fmt.Errorf("jobs.NewWorker: LeaseDuration (%v) must be at least 3x HeartbeatInterval (%v)",
			cfg.LeaseDuration, cfg.HeartbeatInterval)
	}
	return &Worker{
		manager: m,
		cfg:     cfg,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		sem:     make(chan struct{}, cfg.Concurrency),
	}, nil
}

// ID returns the worker's identity, as persisted in locked_by.
func (w *Worker) ID() string { return w.cfg.ID }

// Start blocks until ctx is cancelled or [Worker.Stop] is called.
// The convention mirrors http.Server.ListenAndServe: launch in its
// own goroutine when other work needs to continue.
//
// While running, the worker also performs periodic housekeeping
// once per [WorkerConfig.SweepInterval]: reclaim jobs whose leases
// expired, reap worker rows from crashed peers, and refresh its
// own LastSeenAt so [Manager.ListWorkers] shows it as alive.
//
// On shutdown, polling stops immediately and in-flight jobs continue
// until they finish or the drain context (the Stop ctx, or the
// Start ctx on direct cancel) expires; on expiry, the runtime
// cancels each job's context. Jobs that ignore cancellation will
// outrun shutdown; their leases expire and another worker reclaims
// them.
//
// Return value:
//   - nil when in-flight jobs drained cleanly before the drain
//     context expired.
//   - ctx.Err() of the drain context otherwise: typically
//     [context.Canceled] (the Start ctx was cancelled, or Stop was
//     called with an already-cancelled ctx) or
//     [context.DeadlineExceeded] (Stop was called with a deadline
//     and in-flight jobs ran past it).
//
// Callers usually filter context.Canceled out as the normal
// shutdown path:
//
//	if err := worker.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
//	    log.Error("worker exited", "err", err)
//	}
func (w *Worker) Start(ctx context.Context) error {
	defer close(w.doneCh)

	// Register self so ListWorkers can see us. Failure here is
	// non-fatal: the worker still runs, but operators will not see
	// it in the UI until the next sweep tick refreshes the row.
	startedAt := time.Now().UTC()
	host, _ := os.Hostname()
	regCtx, regCancel := w.manager.withStoreTimeout(ctx)
	if err := w.manager.store.UpsertWorker(regCtx, &WorkerRow{
		ID:         w.cfg.ID,
		Hostname:   host,
		Queues:     w.cfg.Queues,
		StartedAt:  startedAt,
		LastSeenAt: startedAt,
	}); err != nil {
		w.manager.config.Logger.Warn("jobs: worker register failed",
			"worker", w.cfg.ID, "err", err)
	}
	regCancel()
	defer func() {
		// Retire on a fresh background ctx (the caller's ctx may
		// already be cancelled by the time the deferred drain finishes).
		retCtx, retCancel := w.manager.withStoreTimeout(context.Background())
		defer retCancel()
		_ = w.manager.store.RetireWorker(retCtx, w.cfg.ID)
	}()

	poll := time.NewTicker(w.cfg.PollInterval)
	defer poll.Stop()
	sweep := time.NewTicker(w.cfg.SweepInterval)
	defer sweep.Stop()

	var drainCtx context.Context
loop:
	for {
		select {
		case <-ctx.Done():
			drainCtx = ctx
			break loop
		case <-w.stopCh:
			w.drainMu.Lock()
			drainCtx = w.drainCtx
			w.drainMu.Unlock()
			if drainCtx == nil {
				drainCtx = context.Background()
			}
			break loop
		case <-poll.C:
			w.tryClaim(ctx)
		case <-sweep.C:
			w.trySweep(ctx)
		}
	}
	return w.drain(drainCtx)
}

// Stop signals shutdown and blocks until Start returns. ctx is the
// drain deadline: in-flight jobs run until they finish or ctx
// expires, at which point their contexts are cancelled. Passing
// context.Background() drains forever.
func (w *Worker) Stop(ctx context.Context) {
	w.stopOnce.Do(func() {
		w.drainMu.Lock()
		w.drainCtx = ctx
		w.drainMu.Unlock()
		close(w.stopCh)
	})
	<-w.doneCh
}

// tryClaim asks the store for as many jobs as the worker can take
// right now, and dispatches each to a goroutine.
func (w *Worker) tryClaim(ctx context.Context) {
	available := cap(w.sem) - len(w.sem)
	if available <= 0 {
		return
	}
	queues, budget := w.eligibleQueues()
	if len(queues) == 0 {
		// Every configured queue is at its per-queue cap. Skipping
		// the Claim entirely (instead of asking with an empty queue
		// list) avoids a no-op round trip to the store.
		return
	}
	claimCtx, cancel := w.manager.withStoreTimeout(ctx)
	rows, err := w.manager.store.Claim(claimCtx, ClaimRequest{
		WorkerID:      w.cfg.ID,
		Queues:        queues,
		Now:           time.Now().UTC(),
		LeaseDuration: w.cfg.LeaseDuration,
		Limit:         available,
		QueueLimits:   budget,
		KindLimits:    w.manager.snapshotKindLimits(),
	})
	cancel()
	if err != nil {
		w.manager.config.Logger.Error("jobs: claim failed",
			"worker", w.cfg.ID, "err", err)
		return
	}
	for _, row := range rows {
		// Reserve a slot first; if Start is shutting down, the
		// poll select would have exited before we got here.
		w.sem <- struct{}{}
		w.bumpQueueInFlight(row.Queue, 1)
		w.wg.Add(1)
		go w.run(row)
	}
}

// eligibleQueues returns the queues this worker may pull from on
// the next claim cycle, alongside the per-queue budget map. Queues
// at their PerQueue cap are dropped from the returned slice so the
// store's SELECT does not waste its LIMIT on candidates that would
// be filtered out anyway (which would starve other queues when the
// at-cap queue has higher-priority work).
//
// When no PerQueue caps are configured, returns the worker's full
// queue list and a nil budget (the store treats nil as unlimited).
func (w *Worker) eligibleQueues() (queues []string, budget map[string]int) {
	if len(w.cfg.PerQueue) == 0 {
		return w.cfg.Queues, nil
	}
	budget = make(map[string]int, len(w.cfg.PerQueue))
	queues = make([]string, 0, len(w.cfg.Queues))
	for _, q := range w.cfg.Queues {
		limit, capped := w.cfg.PerQueue[q]
		if !capped || limit <= 0 {
			// Uncapped queue: always eligible, no budget entry.
			queues = append(queues, q)
			continue
		}
		remaining := limit - int(w.queueInFlightCount(q))
		if remaining <= 0 {
			// Saturated: skip entirely so the SELECT does not see
			// its rows.
			continue
		}
		queues = append(queues, q)
		budget[q] = remaining
	}
	return queues, budget
}

func (w *Worker) bumpQueueInFlight(queue string, delta int32) {
	v, _ := w.queueInFlight.LoadOrStore(queue, new(atomic.Int32))
	v.(*atomic.Int32).Add(delta)
}

func (w *Worker) queueInFlightCount(queue string) int32 {
	if v, ok := w.queueInFlight.Load(queue); ok {
		return v.(*atomic.Int32).Load()
	}
	return 0
}

// trySweep performs the three periodic housekeeping tasks: reclaim
// jobs whose leases have expired, remove worker rows left behind
// by crashed workers (kill -9, OOM), and refresh this worker's own
// LastSeenAt so it stays in ListWorkers.
func (w *Worker) trySweep(ctx context.Context) {
	sweepCtx, sweepCancel := w.manager.withStoreTimeout(ctx)
	n, err := w.manager.store.SweepExpired(sweepCtx, time.Now().UTC())
	sweepCancel()
	if err != nil {
		w.manager.config.Logger.Error("jobs: sweep failed",
			"worker", w.cfg.ID, "err", err)
		return
	}
	if n > 0 {
		w.manager.config.Logger.Info("jobs: reclaimed expired jobs",
			"worker", w.cfg.ID, "count", n)
	}

	// Stale-worker sweep: any worker row whose LastSeenAt is older
	// than staleWorkerMultiplier * LeaseDuration is treated as
	// dead. Alive workers refresh LastSeenAt every SweepInterval
	// (configured << LeaseDuration), so the multiplier leaves
	// plenty of headroom for an alive worker that briefly stops
	// sweeping (long-running job, GC pause).
	staleBefore := time.Now().Add(-staleWorkerMultiplier * w.cfg.LeaseDuration).UTC()
	staleCtx, staleCancel := w.manager.withStoreTimeout(ctx)
	removed, err := w.manager.store.SweepStaleWorkers(staleCtx, staleBefore)
	staleCancel()
	if err != nil {
		w.manager.config.Logger.Warn("jobs: stale-worker sweep failed",
			"worker", w.cfg.ID, "err", err)
	} else if removed > 0 {
		w.manager.config.Logger.Info("jobs: removed stale workers",
			"worker", w.cfg.ID, "count", removed)
	}

	now := time.Now().UTC()
	host, _ := os.Hostname()
	// StartedAt is set once by the initial UpsertWorker in Start();
	// every backend's UpsertWorker preserves it on subsequent
	// upserts, so this call only refreshes LastSeenAt.
	renewCtx, renewCancel := w.manager.withStoreTimeout(ctx)
	_ = w.manager.store.UpsertWorker(renewCtx, &WorkerRow{
		ID:         w.cfg.ID,
		Hostname:   host,
		Queues:     w.cfg.Queues,
		StartedAt:  now,
		LastSeenAt: now,
	})
	renewCancel()
}

// staleWorkerMultiplier controls when a worker row is treated as
// abandoned (LastSeenAt older than staleWorkerMultiplier * the
// sweeping worker's LeaseDuration). 5 is comfortably above the
// sweep cadence so an alive worker that briefly stops sweeping
// (long-running job, GC pause, transient network blip) is not
// reaped, while a crashed worker disappears within a minute under
// default settings (60s lease * 5 = 5min, but bounded by next
// sweep tick after the cutoff passes).
const staleWorkerMultiplier = 5

// drain waits for in-flight jobs to finish, cancelling their
// contexts when ctx expires.
func (w *Worker) drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		w.cancels.Range(func(_, v any) bool {
			if cancel, ok := v.(context.CancelFunc); ok {
				cancel()
			}
			return true
		})
		<-done
		return ctx.Err()
	}
}

// workerID returns a per-process identifier. Hostname + PID + a
// random suffix is enough for ListWorkers output and lease
// attribution; full uniqueness is not required because the lease
// uses (workerID, jobID) and the JobRow id is already unique.
func workerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), NewID()[:8])
}

// run wraps a single job attempt. Builds the per-job context (with
// timeout if configured), starts the heartbeat goroutine, invokes
// Run with panic recovery, then writes the attempt ledger row and
// the outcome.
func (w *Worker) run(row *JobRow) {
	defer w.wg.Done()
	defer func() { <-w.sem }()
	defer w.bumpQueueInFlight(row.Queue, -1)

	// Long-lived background ctx for store calls that should outlive
	// the per-job ctx (heartbeat retries, final Complete write).
	bg := context.Background()

	attemptNum := row.Attempt + 1

	// An unregistered kind is a terminal park: we cannot decode the
	// payload or even build a [Context]. Record the attempt and the
	// outcome without ever taking the lease lifecycle path.
	w.manager.mu.RLock()
	ctor, ok := w.manager.constructorByName[row.Kind]
	w.manager.mu.RUnlock()
	if !ok {
		unregErr := fmt.Errorf("%w: %s", ErrUnregistered, row.Kind)
		w.recordImmediate(bg, row, attemptNum, AttemptFailed, unregErr, Outcome{
			State: StateFailed, Attempt: attemptNum, Error: unregErr.Error(),
		})
		return
	}

	job := ctor()
	if err := decodePayload(row.Payload, job); err != nil {
		w.recordImmediate(bg, row, attemptNum, AttemptFailed, err, Outcome{
			State: StateFailed, Attempt: attemptNum, Error: err.Error(),
		})
		return
	}

	// Per-job context: cancellable so heartbeat can force-cancel on
	// a cancellation request, and timeout-bounded when configured.
	runCtx, cancel := context.WithCancel(bg)
	defer cancel()
	if row.TimeoutMs > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(runCtx, time.Duration(row.TimeoutMs)*time.Millisecond)
		defer timeoutCancel()
	}
	w.cancels.Store(row.ID, cancel)
	defer w.cancels.Delete(row.ID)

	state := &jobState{
		jobID:   row.ID,
		kind:    row.Kind,
		attempt: attemptNum,
		logger:  w.manager.config.Logger.With("job_id", row.ID, "kind", row.Kind),
		store:   w.manager.store,
	}
	jc := &jobCtx{Context: runCtx, state: state}

	// Heartbeat extends the lease and observes cancel requests.
	// Progress flusher coalesces ctx.Progress writes into the
	// store. Both must be quiesced before we read jobState fields
	// (cancelByUser, lastProgress) for the outcome decision below;
	// a late-firing heartbeat could otherwise set cancelByUser
	// after the runner already read it, dropping the cancellation
	// signal. We track them in a WaitGroup and wait synchronously
	// right after Run returns; the deferred hbStop/pfStop are
	// safety nets for the panic-escape path.
	var auxWg sync.WaitGroup

	hbCtx, hbStop := context.WithCancel(bg)
	defer hbStop()
	auxWg.Add(1)
	go func() {
		defer auxWg.Done()
		w.heartbeat(hbCtx, row.ID, state, cancel)
	}()

	pfCtx, pfStop := context.WithCancel(bg)
	defer pfStop()
	auxWg.Add(1)
	go func() {
		defer auxWg.Done()
		w.flushProgress(pfCtx, state)
	}()

	// Hooks fire around Run. OnFinish is deferred so it always
	// emits, even if a subsequent panic escapes safeRun or the
	// outcome bookkeeping below blows up.
	startEvent := StartEvent{
		JobID:   state.jobID,
		Kind:    state.kind,
		Attempt: state.attempt,
		Logger:  state.logger,
	}
	started := time.Now().UTC()
	var runErr error
	defer func() {
		if w.manager.config.Hooks.OnFinish != nil {
			w.manager.safeHook("OnFinish", func() {
				w.manager.config.Hooks.OnFinish(runCtx, FinishEvent{
					StartEvent: startEvent,
					Err:        runErr,
					Dur:        time.Since(started),
				})
			})
		}
	}()
	if w.manager.config.Hooks.OnStart != nil {
		w.manager.safeHook("OnStart", func() {
			w.manager.config.Hooks.OnStart(runCtx, startEvent)
		})
	}
	runErr = safeRun(jc, job)
	finished := time.Now().UTC()

	// Quiesce the helper goroutines BEFORE reading jobState. This
	// guarantees that any cancel_requested flag the heartbeat ever
	// observed has been written to state.cancelByUser, and any
	// pending progress update has been processed.
	hbStop()
	pfStop()
	auxWg.Wait()

	outcome := Outcome{Attempt: attemptNum}
	state.mu.Lock()
	if state.progressDirty {
		p := state.lastProgress
		outcome.FinalProgress = &p
	}
	state.mu.Unlock()

	attemptState := w.decideOutcome(row, runErr, &outcome, state)

	// Complete runs BEFORE RecordAttempt so we can mirror any
	// state override (e.g. the cancel-then-success race where the
	// store rewrites StateSucceeded to StateCancelled because
	// cancel_requested was set on the row) into the ledger.
	//
	// Trade-off: a failure of recordAttempt below (rare: network
	// blip after Complete commits) leaves the ledger missing one
	// row while the job state is correctly terminal. State
	// correctness wins over ledger completeness; operators
	// investigating "succeeded job with one fewer attempt than
	// runs" should check worker logs for the warning.
	completeCtx, completeCancel := w.manager.withStoreTimeout(bg)
	applied, err := w.manager.store.Complete(completeCtx, row.ID, w.cfg.ID, outcome)
	completeCancel()
	if err != nil {
		w.manager.config.Logger.Warn("jobs: complete dropped",
			"worker", w.cfg.ID, "job_id", row.ID, "err", err)
		// No attempt row either: without a successful Complete, the
		// runner is forfeiting this attempt entirely.
		return
	}
	if applied != outcome.State {
		w.manager.config.Logger.Info("jobs: outcome overridden by store",
			"worker", w.cfg.ID, "job_id", row.ID,
			"requested", outcome.State, "applied", applied)
		attemptState = attemptStateFor(applied, attemptState)
		outcome.State = applied
	}

	w.recordAttempt(bg, &Attempt{
		ID:         NewID(),
		JobID:      row.ID,
		Attempt:    attemptNum,
		WorkerID:   w.cfg.ID,
		StartedAt:  started,
		FinishedAt: finished,
		State:      attemptState,
		Error:      outcome.Error,
	})
}

// attemptStateFor maps a final job [State] to its corresponding
// [AttemptState]. Used by the runner when the store overrides the
// outcome (cancel-then-success race) so the ledger matches the
// persisted job state. fallback is returned for terminal States
// that have no distinct attempt mapping (today: discarded).
func attemptStateFor(s State, fallback AttemptState) AttemptState {
	switch s {
	case StateSucceeded:
		return AttemptSucceeded
	case StateCancelled:
		return AttemptCancelled
	case StateFailed, StateDiscarded:
		return AttemptFailed
	}
	return fallback
}

// decideOutcome fills in outcome.State / Error / AvailableAt based
// on runErr and row metadata, and returns the corresponding
// [AttemptState] for the ledger.
func (w *Worker) decideOutcome(row *JobRow, runErr error, outcome *Outcome, state *jobState) AttemptState {
	attemptNum := outcome.Attempt
	if runErr == nil {
		outcome.State = StateSucceeded
		return AttemptSucceeded
	}
	outcome.Error = runErr.Error()

	// Cancellation observed via the heartbeat goroutine takes
	// precedence over the generic context.Canceled path: a user
	// CancelJob produces a terminal cancelled state, whereas a
	// shutdown-triggered cancel falls through to the retry logic.
	state.mu.Lock()
	cancelledByUser := state.cancelByUser
	state.mu.Unlock()
	if cancelledByUser {
		outcome.State = StateCancelled
		return AttemptCancelled
	}

	switch {
	case errors.Is(runErr, ErrPermanent):
		// Non-retryable: straight to failed, regardless of remaining attempts.
		outcome.State = StateFailed
		return AttemptFailed

	case errors.Is(runErr, context.DeadlineExceeded) && row.TimeoutMs > 0:
		// Per-attempt timeout expired. Dispatch via the persisted
		// OnTimeout value the user attached at enqueue time.
		return w.dispatchTimeout(row, attemptNum, outcome)

	case attemptNum >= row.MaxAttempts:
		outcome.State = StateDiscarded
		return AttemptFailed

	default:
		outcome.State = StateScheduled
		outcome.AvailableAt = time.Now().Add(w.backoffFor(row).Next(attemptNum)).UTC()
		return AttemptFailed
	}
}

func (w *Worker) dispatchTimeout(row *JobRow, attemptNum int, outcome *Outcome) AttemptState {
	switch OnTimeout(row.OnTimeoutInt) {
	case TimeoutFail:
		outcome.State = StateFailed
	case TimeoutDiscard:
		outcome.State = StateDiscarded
	default: // TimeoutRetry
		if attemptNum >= row.MaxAttempts {
			outcome.State = StateDiscarded
		} else {
			outcome.State = StateScheduled
			outcome.AvailableAt = time.Now().Add(w.backoffFor(row).Next(attemptNum)).UTC()
		}
	}
	return AttemptTimedOut
}

// backoffFor returns the Backoff that should drive the next retry
// for row: the per-job override (decoded from row.BackoffSpec) when
// present, otherwise the manager's DefaultBackoff.
func (w *Worker) backoffFor(row *JobRow) Backoff {
	if b := decodeBackoff(row.BackoffSpec); b != nil {
		return b
	}
	return w.manager.config.DefaultBackoff
}

// recordImmediate is the unregistered-kind / decode-failure path:
// log the attempt and write the terminal outcome with no heartbeat
// or per-job context. OnStart and OnFinish still fire with a
// synthetic event so observability tools count these attempts
// alongside normal-path failures; Dur is 0 since no Run was invoked.
func (w *Worker) recordImmediate(ctx context.Context, row *JobRow, attemptNum int, s AttemptState, runErr error, o Outcome) {
	startEvent := StartEvent{
		JobID:   row.ID,
		Kind:    row.Kind,
		Attempt: attemptNum,
		Logger:  w.manager.config.Logger.With("job_id", row.ID, "kind", row.Kind),
	}
	if w.manager.config.Hooks.OnStart != nil {
		w.manager.safeHook("OnStart", func() {
			w.manager.config.Hooks.OnStart(ctx, startEvent)
		})
	}
	// OnFinish is deferred so it always fires, including when
	// recordAttempt or Complete blow up.
	defer func() {
		if w.manager.config.Hooks.OnFinish != nil {
			w.manager.safeHook("OnFinish", func() {
				w.manager.config.Hooks.OnFinish(ctx, FinishEvent{
					StartEvent: startEvent,
					Err:        runErr,
					Dur:        0, // no Run invoked
				})
			})
		}
	}()

	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	now := time.Now().UTC()
	w.recordAttempt(ctx, &Attempt{
		ID:         NewID(),
		JobID:      row.ID,
		Attempt:    attemptNum,
		WorkerID:   w.cfg.ID,
		StartedAt:  now,
		FinishedAt: now,
		State:      s,
		Error:      errMsg,
	})
	completeCtx, cancel := w.manager.withStoreTimeout(ctx)
	defer cancel()
	if _, err := w.manager.store.Complete(completeCtx, row.ID, w.cfg.ID, o); err != nil {
		w.manager.config.Logger.Warn("jobs: complete-immediate dropped",
			"worker", w.cfg.ID, "job_id", row.ID, "err", err)
	}
}

func (w *Worker) recordAttempt(ctx context.Context, a *Attempt) {
	storeCtx, cancel := w.manager.withStoreTimeout(ctx)
	defer cancel()
	if err := w.manager.store.RecordAttempt(storeCtx, a); err != nil {
		w.manager.config.Logger.Warn("jobs: record-attempt failed",
			"worker", w.cfg.ID, "job_id", a.JobID, "err", err)
	}
}

func (w *Worker) heartbeat(ctx context.Context, jobID string, state *jobState, cancel context.CancelFunc) {
	ticker := time.NewTicker(w.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Parent on ctx (the heartbeat lifetime) so an in-flight
			// store call is interrupted when the runner calls hbStop
			// after Run returns. Without this, auxWg.Wait() could
			// block for up to StoreTimeout against a hung backend.
			hbCallCtx, hbCallCancel := w.manager.withStoreTimeout(ctx)
			cancelRequested, err := w.manager.store.Heartbeat(
				hbCallCtx,
				jobID, w.cfg.ID,
				time.Now().Add(w.cfg.LeaseDuration).UTC(),
			)
			hbCallCancel()
			if err != nil {
				// ctx already cancelled: this is the clean-shutdown
				// race (hbStop fired mid-call). No warning, just exit.
				if ctx.Err() != nil {
					return
				}
				// Lost the lease or the row is gone; cancel the
				// run so it stops doing useless work.
				if errors.Is(err, ErrNotFound) {
					cancel()
					return
				}
				w.manager.config.Logger.Warn("jobs: heartbeat failed",
					"worker", w.cfg.ID, "job_id", jobID, "err", err)
				continue
			}
			if cancelRequested {
				// Record the cause so the runner can pick the right
				// terminal state (cancelled vs scheduled-retry).
				state.mu.Lock()
				state.cancelByUser = true
				state.mu.Unlock()
				cancel()
				return
			}
		}
	}
}

// safeRun converts panics into errors so a buggy handler does not
// take the worker down. The format matches the middleware.Recover
// convention from the cli library for cross-tool consistency.
func safeRun(ctx Context, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return j.Run(ctx)
}

// progressFlushInterval bounds how often the throttle goroutine can
// write to the store. Pinned at 500ms per the spec.
const progressFlushInterval = 500 * time.Millisecond

// flushProgress ticks every progressFlushInterval and writes the
// current progress cell to the store iff it is dirty. The runner
// also performs a final flush before Complete; both paths share the
// same dirty-and-snapshot dance to avoid duplicate writes.
func (w *Worker) flushProgress(ctx context.Context, state *jobState) {
	ticker := time.NewTicker(progressFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tryFlushProgress(ctx, state)
		}
	}
}

func (w *Worker) tryFlushProgress(ctx context.Context, state *jobState) {
	state.mu.Lock()
	if !state.progressDirty {
		state.mu.Unlock()
		return
	}
	snap := state.lastProgress
	state.progressDirty = false
	state.mu.Unlock()

	// Parent on ctx (the flusher lifetime) so an in-flight store call
	// is interrupted when the runner calls pfStop after Run returns.
	// Without this, auxWg.Wait() could block for up to StoreTimeout
	// against a hung backend.
	upCtx, upCancel := w.manager.withStoreTimeout(ctx)
	defer upCancel()
	if err := w.manager.store.UpdateProgress(upCtx,
		state.jobID, snap.Done, snap.Total, snap.Msg); err != nil {
		// ctx already cancelled: clean-shutdown race (pfStop fired
		// mid-call). Re-mark dirty so the runner's final-flush via
		// Outcome.FinalProgress picks up the snap; otherwise this
		// update is silently lost. A concurrent user Progress() may
		// have already overwritten lastProgress with a newer value;
		// dirty=true tells the runner "there's something to flush"
		// either way.
		if ctx.Err() != nil {
			state.mu.Lock()
			state.progressDirty = true
			state.mu.Unlock()
			return
		}
		// Lost the lease or the job is gone; the next heartbeat
		// will discover the same and stop the run.
		w.manager.config.Logger.Warn("jobs: progress flush failed",
			"worker", w.cfg.ID, "job_id", state.jobID, "err", err)
	}
}
