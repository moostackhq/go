package jobs

import (
	"context"
	"time"
)

// Store is the seam between the jobs runtime and persistence.
// Concrete implementations live in sibling subpackages:
//
//   - github.com/moostackhq/go/jobs/store/memory   non-durable, for tests/demos
//   - github.com/moostackhq/go/jobs/store/sqlite   single-node
//   - github.com/moostackhq/go/jobs/store/postgres distributed, production
//
// Backends are expected to be safe for concurrent use across many
// goroutines. Each method is atomic from the caller's perspective;
// backends may use any internal transaction model.
type Store interface {
	// Insert persists a new job row. The row's ID must be set by
	// the caller before Insert returns successfully.
	Insert(ctx context.Context, row *JobRow) error

	// InsertTx writes a job inside the caller's transaction. The tx
	// parameter is backend-specific (*sql.Tx for SQL backends); the
	// memory backend returns [ErrUnsupported].
	InsertTx(ctx context.Context, tx any, row *JobRow) error

	// Get returns a job by id, or [ErrNotFound] if missing.
	Get(ctx context.Context, id string) (*JobRow, error)

	// List returns a page of jobs matching the filter. The returned
	// nextCursor is empty when the page is the last one.
	List(ctx context.Context, f JobFilter) (rows []*JobRow, nextCursor string, err error)

	// Claim atomically locks up to req.Limit eligible jobs from the
	// requested queues and returns them as running, with locked_by =
	// req.WorkerID and locked_until = req.Now + req.LeaseDuration.
	// Eligibility is: state in (available, scheduled) AND
	// available_at <= req.Now AND queue IN req.Queues.
	Claim(ctx context.Context, req ClaimRequest) ([]*JobRow, error)

	// Heartbeat extends a running job's lease, returning whether a
	// cancellation has been requested since the last call. Returns
	// [ErrNotFound] if the job has already been completed or
	// reclaimed by another worker.
	Heartbeat(ctx context.Context, jobID, workerID string, until time.Time) (cancelRequested bool, err error)

	// Complete writes the final outcome of an attempt and clears the
	// lease. Backends must reject the call if the job is no longer
	// held by the supplied workerID (lease stolen via expiry); the
	// runner handles [ErrNotFound] by abandoning the result.
	//
	// The returned State is the state the backend actually wrote,
	// which may differ from outcome.State when the runner won a
	// race against a concurrent [Manager.CancelJob]: if the row's
	// CancelRequested flag is set AND outcome.State is
	// [StateSucceeded], the backend writes [StateCancelled]
	// instead and returns that. The runner mirrors the override
	// into the attempt ledger so the inspection API stays
	// consistent with the persisted job state.
	Complete(ctx context.Context, jobID, workerID string, outcome Outcome) (applied State, err error)

	// SweepExpired transitions running jobs whose locked_until is
	// in the past back to available so another worker can claim
	// them. For each reclaimed job, the store also writes a
	// synthetic [Attempt] row (State=AttemptFailed,
	// Error="lease expired") attributed to the previous locked_by
	// and increments the row's Attempt counter, so the ledger
	// shows the crashed attempt rather than a gap. Returns the
	// number of jobs reclaimed.
	SweepExpired(ctx context.Context, now time.Time) (int, error)

	// RecordAttempt persists one row of the `job_attempts` ledger.
	// The runner calls this exactly once per invocation of Run,
	// before Complete.
	RecordAttempt(ctx context.Context, a *Attempt) error

	// ListAttempts returns up to limit attempts for jobID whose
	// Attempt number is strictly greater than afterAttempt, ordered
	// by Attempt ascending. The caller passes limit+1 and uses the
	// extra row to detect "more pages exist." Returns an empty
	// slice (not an error) when no matching attempts exist.
	ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]*Attempt, error)

	// GetStep returns the persisted record for (jobID, name), or
	// [ErrNotFound] if no such step has been completed.
	GetStep(ctx context.Context, jobID, name string) (*StepRecord, error)

	// SaveStep persists a completed step result. SQL backends
	// enforce (job_id, name) uniqueness; the runner only calls
	// SaveStep after Step has verified the row does not exist, so a
	// duplicate-key error here implies a concurrent attempt and
	// is a hard failure.
	SaveStep(ctx context.Context, s *StepRecord) error

	// ListSteps returns the persisted step records for a job, in
	// insertion order.
	ListSteps(ctx context.Context, jobID string) ([]*StepRecord, error)

	// UpdateProgress writes the latest progress cell for a job. The
	// runner's throttle goroutine calls this; user code does not.
	UpdateProgress(ctx context.Context, jobID string, done, total int64, msg string) error

	// Retry resets a terminal job (failed, discarded, or cancelled)
	// to available so it can be picked up again. Returns
	// [ErrJobNotRetryable] when the job is in a non-terminal state.
	// The attempt counter is preserved; the cap is bumped so one
	// additional attempt is permitted.
	Retry(ctx context.Context, jobID string, now time.Time) error

	// Cancel marks a job as cancelled. Behaviour depends on the
	// current state: scheduled or available jobs land in
	// [StateCancelled] immediately; running jobs have their
	// CancelRequested flag set so the next [Heartbeat] returns true
	// and the runner can finish gracefully. Terminal jobs return
	// [ErrJobTerminal]. The bool result indicates whether the cancel
	// took effect immediately (false = pending worker observation).
	Cancel(ctx context.Context, jobID string, now time.Time) (immediate bool, err error)

	// Delete removes a job (and its steps and attempts via FK
	// cascade). Returns [ErrJobRunning] if the job is currently
	// leased; the caller must Cancel first and wait for the worker
	// to release the lease.
	Delete(ctx context.Context, jobID string) error

	// UpsertWorker registers a worker as alive. Called by
	// [Worker.Start] once at startup and every heartbeat tick to
	// refresh LastSeenAt.
	UpsertWorker(ctx context.Context, w *WorkerRow) error

	// RetireWorker removes a worker row. Called by [Worker.Stop]
	// after the drain completes.
	RetireWorker(ctx context.Context, workerID string) error

	// ListWorkers returns all alive workers, ordered by StartedAt.
	ListWorkers(ctx context.Context) ([]*WorkerRow, error)

	// SweepStaleWorkers removes workers whose LastSeenAt is older
	// than the supplied cutoff. Used by the worker run loop to
	// reap rows left behind by crashed workers (kill -9, OOM)
	// that never called [Worker.Stop]. Returns the number removed.
	SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error)

	// ListQueues returns one entry per distinct queue, with counts
	// per state. Order is alphabetical by name.
	ListQueues(ctx context.Context) ([]QueueInfo, error)

	// UpsertSchedule inserts or updates a schedule by Name.
	UpsertSchedule(ctx context.Context, s *ScheduleRow) error

	// DeleteSchedule removes a schedule by Name. No-op when absent.
	DeleteSchedule(ctx context.Context, name string) error

	// ListSchedules returns every persisted schedule, ordered by Name.
	ListSchedules(ctx context.Context) ([]*ScheduleRow, error)

	// DueSchedules returns every schedule whose NextRunAt is at or
	// before now.
	DueSchedules(ctx context.Context, now time.Time) ([]*ScheduleRow, error)

	// ClaimSchedule advances the schedule's LastRunAt and NextRunAt
	// IFF the stored LastRunAt still equals expectedLastRunAt.
	// Returns claimed=true on the winner of a racing-schedulers
	// scenario, false on losers. Multiple processes calling
	// StartScheduler are therefore safe.
	ClaimSchedule(ctx context.Context, name string, expectedLastRunAt, newLastRunAt, newNextRunAt time.Time) (claimed bool, err error)
}

// JobRow is the persistence shape of a job. It mirrors the columns
// of the `jobs` table in the SQL schemas; the memory store keeps
// these in a map.
type JobRow struct {
	ID          string
	Kind        string
	Payload     []byte
	Queue       string
	Priority    int
	State       State
	Attempt     int
	MaxAttempts int
	AvailableAt time.Time
	// TimeoutMs is the per-attempt timeout in milliseconds; zero
	// means no timeout. Stored as bigint for SQLite portability.
	TimeoutMs int64
	// OnTimeoutInt is the persisted [OnTimeout] value; runner uses
	// it to decide what to do when a per-attempt timeout fires.
	OnTimeoutInt int
	// BackoffSpec is the JSON-encoded per-job [Backoff] override
	// (nil = use the manager's DefaultBackoff on retry). Today only
	// [ExponentialBackoff] is serialisable; any other concrete
	// Backoff supplied via [Options.Backoff] is silently dropped.
	BackoffSpec   []byte
	UniqueKey     string
	ProgressDone  int64
	ProgressTotal int64
	ProgressMsg   string
	Error         string
	LockedBy      string
	LockedUntil   time.Time
	HeartbeatAt   time.Time
	// CancelRequested is set by [Manager.CancelJob] on a running job;
	// the worker observes it on the next heartbeat and cancels the
	// job's context.
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ClaimRequest is the parameter to [Store.Claim]. WorkerID and Now
// are required; Limit defaults to 1 when zero.
type ClaimRequest struct {
	WorkerID      string
	Queues        []string
	Now           time.Time
	LeaseDuration time.Duration
	Limit         int

	// QueueLimits caps how many jobs the store may return from each
	// listed queue in this call. The worker computes this from its
	// PerQueue config minus current in-flight, so the store can
	// stay agnostic to per-worker bookkeeping. A queue with no
	// entry is unlimited; -1 explicitly is also unlimited.
	QueueLimits map[string]int

	// KindLimits caps how many jobs of each kind may be running
	// globally at any time. The store counts current running jobs
	// per kind and skips candidates that would push past the cap.
	// A kind with no entry is unlimited.
	KindLimits map[string]int
}

// WorkerRow is the persistence shape of a worker row, written by
// [Worker.Start] and read back by [Manager.ListWorkers].
type WorkerRow struct {
	ID         string
	Hostname   string
	Queues     []string
	StartedAt  time.Time
	LastSeenAt time.Time
}

// QueueInfo describes one queue and its current job counts. The
// totals reflect every state, not just claimable jobs.
type QueueInfo struct {
	Name   string
	Counts map[State]int
}

// ScheduleRow is the persistence shape of a cron-style schedule.
// OptionsJSON holds the marshalled [ScheduleOptions]; Payload holds
// the JSON of the prototype job the scheduler enqueues on each fire.
type ScheduleRow struct {
	Name        string
	Kind        string
	Cron        string
	Payload     []byte
	OptionsJSON []byte
	NextRunAt   time.Time
	LastRunAt   time.Time
	UpdatedAt   time.Time
}

// Outcome is the parameter to [Store.Complete]. The runner builds it
// from the result of [Job.Run]; the store applies it atomically.
type Outcome struct {
	// State is the new terminal/intermediate state. Valid values
	// are succeeded, failed, discarded, cancelled, or scheduled
	// (for a retry).
	State State
	// Attempt is the new attempt count to persist (the runner
	// computes it as row.Attempt+1 before calling Complete).
	Attempt int
	// AvailableAt is the next claimable time when State is
	// scheduled. Ignored for terminal states.
	AvailableAt time.Time
	// Error is the human-readable error message, persisted on the
	// row's `error` column. Empty on success.
	Error string
	// FinalProgress, when non-nil, is flushed before the state
	// transition so the last reported progress is visible to
	// observers. The runner sets it from the final ctx.Progress
	// call when one was made, otherwise leaves it nil.
	FinalProgress *Progress
}
