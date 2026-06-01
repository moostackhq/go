package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Hooks attach observability without the core depending on any
// concrete metrics or tracing library. Nil fields are skipped. All
// three hooks share the same call shape: `func(ctx, eventStruct)`.
// OnFinish runs via defer so it always fires, including for
// panic-converted errors and context cancellations.
type Hooks struct {
	OnEnqueue func(ctx context.Context, e EnqueueEvent)
	OnStart   func(ctx context.Context, e StartEvent)
	OnFinish  func(ctx context.Context, e FinishEvent)
}

// EnqueueEvent describes a freshly-enqueued job, passed to
// [Hooks.OnEnqueue].
type EnqueueEvent struct {
	// JobID is the id assigned to the new row.
	JobID string
	// Kind is the registered name the job was enqueued under.
	Kind string
	// Job is the user's struct value, as supplied to Enqueue. The
	// hook receives the live pointer; do not mutate.
	Job Job
	// Opts is the resolved [Options] after manager defaults were
	// applied (queue, MaxAttempts, Backoff). Reading Opts.Backoff
	// here tells you what schedule the job will use on retry,
	// whether the user supplied a per-job override or fell back
	// to [Config.DefaultBackoff].
	Opts Options
	// Tx is true when the enqueue came in via [Manager.EnqueueTx],
	// false for the regular [Manager.Enqueue].
	//
	// The hook fires after a successful InsertTx but BEFORE the
	// caller's transaction commits. A subsequent rollback discards
	// the row, leaving the hook call orphaned — observers that need
	// commit-accurate counts should hook into their own tx commit
	// (e.g., a deferred callback that runs only after a successful
	// Commit) rather than treating OnEnqueue+Tx as authoritative.
	Tx bool
	// ScheduleName is non-empty when the enqueue was produced by
	// [Manager.StartScheduler] firing a registered schedule. Lets
	// hooks distinguish scheduler-driven jobs from caller-driven
	// ones (for tagging, deduplication, telemetry).
	//
	// Tx and ScheduleName are mutually exclusive: scheduler-driven
	// enqueues never participate in a caller's transaction, so
	// Tx==true && ScheduleName!="" is impossible.
	ScheduleName string
}

// StartEvent describes an attempt about to begin, passed to
// [Hooks.OnStart].
type StartEvent struct {
	// JobID identifies the job whose Run is about to be invoked.
	JobID string
	// Kind is the registered name (same string as [Context.Kind]).
	Kind string
	// Attempt is 1 for the first try, 2 for the first retry, and
	// so on.
	Attempt int
	// Logger is the per-job slog.Logger the runtime built (already
	// tagged with job_id and kind).
	Logger *slog.Logger
}

// FinishEvent describes an attempt that just finished, passed to
// [Hooks.OnFinish]. It embeds [StartEvent] so the JobID/Kind/
// Attempt/Logger fields look identical in both hooks.
type FinishEvent struct {
	StartEvent
	// Err is whatever the handler chain returned: nil for success,
	// the user's error, a wrapped panic ("panic: ..."), or a
	// context cancellation. The hook fires exactly once per
	// attempt regardless of how it ended.
	Err error
	// Dur is wall-clock time from the entry of [Job.Run] to its
	// return.
	Dur time.Duration
}
