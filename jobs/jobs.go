// Package jobs is a durable, embeddable job engine for Go. Jobs are
// typed structs with a Run method; the engine persists every state
// transition through a pluggable [store.Store], so workers can crash
// without losing work and pick up where they left off via lease
// expiry. Retries, per-attempt timeouts, durable [Step] resume,
// progress reporting, cron scheduling, and a per-kind concurrency
// limit are all supported out of the box.
//
// Three backends ship with the module: an in-memory store (for tests
// and demos), SQLite (single-node), and PostgreSQL (production,
// distributed workers via SELECT FOR UPDATE SKIP LOCKED).
//
// Jobs are at-least-once: a worker can crash between completing the
// Run and committing the success transition, in which case the job
// runs again. Wrap non-idempotent work in [Step] to make it
// effectively-once across attempts.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Job is the interface every registered job type satisfies. Run is
// invoked on a fresh instance of the registered type, with the
// payload decoded into it.
type Job interface {
	Run(ctx Context) error
}

// State is the lifecycle position of a job. Values match the strings
// persisted in the store's `state` column.
type State string

const (
	StateScheduled State = "scheduled"
	StateAvailable State = "available"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateDiscarded State = "discarded"
)

// Terminal reports whether s is a terminal state. Terminal jobs are
// never re-claimed by a worker and only move via operator action
// ([Manager.RetryJob], [Manager.DeleteJob]).
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateDiscarded:
		return true
	}
	return false
}

// OnTimeout decides what happens to a job that exceeds its
// per-attempt timeout. Default is [TimeoutRetry].
type OnTimeout int

const (
	TimeoutRetry OnTimeout = iota
	TimeoutFail
	TimeoutDiscard
)

// CatchUp decides how the scheduler handles ticks missed during
// downtime, restart, or clock changes. Default is [CatchUpOnce].
type CatchUp int

const (
	// CatchUpOnce fires one job if any tick was missed since the last
	// run, then resumes the regular cadence.
	CatchUpOnce CatchUp = iota
	// CatchUpSkip never fires missed ticks. The schedule resumes
	// from the next future tick.
	CatchUpSkip
	// CatchUpAll fires one job per missed tick. A week of downtime
	// will produce a week of back-to-back enqueues, so opt in
	// knowingly.
	CatchUpAll
)

// Options controls enqueue-time behaviour. The zero value is valid:
// the job lands on the default queue at priority 0 with the
// manager's default backoff and 25 max attempts.
type Options struct {
	Queue    string
	Priority int
	// MaxAttempts is the retry cap. Zero uses
	// [Config.DefaultMaxAttempts] (25 unless set). Negative values
	// cause [Manager.Enqueue] to error: the library is opinionated
	// about bounded retries (use [ErrPermanent] for non-retryable
	// failures, a large explicit cap for "effectively unlimited").
	MaxAttempts int
	Delay       time.Duration
	RunAt       time.Time
	Timeout     time.Duration
	UniqueKey   string
	Backoff     Backoff
	OnTimeout   OnTimeout
}

// Sentinels. Wrap with fmt.Errorf("...: %w", jobs.ErrX) when adding
// context; callers should match with errors.Is.
var (
	// ErrPermanent, when returned (or wrapped) from a job's Run,
	// transitions the job straight to [StateFailed] regardless of
	// remaining attempts.
	ErrPermanent = errors.New("permanent failure, do not retry")

	// ErrUnregistered is set on a job's `error` column when a worker
	// pulls a job whose kind has no registration. The job lands in
	// [StateFailed] without consuming an attempt.
	ErrUnregistered = errors.New("kind is not registered")

	// ErrPayloadDecode is set on a job's `error` column when JSON
	// decode into the registered struct fails. Same handling as
	// [ErrUnregistered].
	ErrPayloadDecode = errors.New("job payload cannot be decoded")

	// ErrJobRunning is returned by [Manager.DeleteJob] when the
	// target job is currently leased. Cancel first, wait for the
	// lease to release, then delete.
	ErrJobRunning = errors.New("job is running")

	// ErrJobTerminal is returned by [Manager.CancelJob] when the
	// target job is already in a terminal state.
	ErrJobTerminal = errors.New("job is in a terminal state")

	// ErrJobNotRetryable is returned by [Manager.RetryJob] when the
	// target job is not in a terminal state.
	ErrJobNotRetryable = errors.New("job is not retryable")

	// ErrNotFound is returned when a lookup by id resolves to no row.
	ErrNotFound = errors.New("not found")

	// ErrUnsupported is returned by store methods that cannot be
	// implemented on the configured backend (for example,
	// [Manager.EnqueueTx] against the memory store).
	ErrUnsupported = errors.New("operation not supported by store")

	// ErrDuplicate is returned by Enqueue when the supplied
	// Options.UniqueKey collides with a non-terminal job of the same
	// kind. The existing job's id is returned alongside the error so
	// callers can ignore the duplicate or surface it.
	ErrDuplicate = errors.New("duplicate unique key")

	// ErrKindAlreadyRegistered is returned by [Register] when the
	// requested name is already bound to a constructor.
	ErrKindAlreadyRegistered = errors.New("kind already registered")
)

// DuplicateError is returned by [Manager.Enqueue] when the supplied
// Options.UniqueKey collides with a non-terminal job of the same
// kind. ExistingID names the live duplicate so callers can decide
// whether to surface it, ignore it, or follow up with [Manager.GetJob].
// errors.Is(err, [ErrDuplicate]) returns true.
type DuplicateError struct {
	ExistingID string
	Kind       string
	UniqueKey  string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("duplicate unique key %q for kind %q (existing job: %s)",
		e.UniqueKey, e.Kind, e.ExistingID)
}

func (e *DuplicateError) Is(target error) bool {
	return target == ErrDuplicate
}

// ctxKey is unexported by design; the only way to obtain a
// [Context] is through the runtime, which constructs and threads it
// for the duration of a Run.
type ctxKey struct{}

// fromStdCtx pulls the per-job state out of a stdlib context. Used
// by [Step]. Returns nil if no job state is attached, which means
// the caller invoked Step from outside a Run.
func fromStdCtx(ctx context.Context) *jobState {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKey{}).(*jobState)
	return v
}

// jobState is the per-run mutable state the runtime attaches to the
// context. Pulled out of context.go so [Step] and the progress /
// step bookkeeping can read it through a plain [context.Context]
// without depending on the concrete jobCtx type.
//
// store is the seam back into persistence: Step uses it to look up
// and save step records; the progress flusher uses it to write
// throttled updates.
type jobState struct {
	jobID   string
	kind    string
	attempt int
	logger  *slog.Logger
	store   Store

	mu            sync.Mutex
	lastProgress  Progress
	progressDirty bool
	// cancelByUser is set by the heartbeat goroutine when a
	// CancelJob request is observed, before it cancels the job's
	// context. The runner reads it after Run returns to distinguish
	// "user cancellation" from "context cancelled by shutdown or
	// lease loss" so it can pick the right terminal state.
	cancelByUser bool
}
