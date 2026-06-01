# jobs

A durable, embeddable job engine for Go. More capable than a simple queue, much simpler than a full workflow engine.

## Features

| Feature | What it gives you |
|---|---|
| Typed jobs | Jobs are Go structs with `Run(jobs.Context) error`. `Register[T]` binds the type at compile time; payload is JSON. No string keys, no reflection magic. |
| Durable steps | Wrap sub-operations in `jobs.Step[T]`; on retry, succeeded steps return their persisted result without re-executing. Void steps use `(any, error)` returning `(nil, ...)`. |
| At-least-once with backoff | Configurable exponential backoff with jitter. Per-job override via `Options.Backoff` persists across crashes. `ErrPermanent` short-circuits to terminal failure. |
| Per-attempt timeouts | `Options.Timeout` cancels the job's context. `OnTimeout` decides retry / fail / discard. |
| Cancellation, retry, delete | `Manager.CancelJob` flips a flag the worker observes via heartbeat (or via `Store.Complete` when the cancel races a successful Run). `RetryJob` revives terminal jobs. `DeleteJob` refuses running ones. |
| Crash-safe ledger | A worker that dies mid-attempt leaves a lease behind; the sweep loop reclaims the job AND writes a synthetic `Attempt{State: failed, Error: "lease expired"}` so the history shows the crash. |
| Self-healing worker registry | Crashed workers (kill -9, OOM) disappear from `ListWorkers` within ~5×LeaseDuration; the run loop reaps stale rows alongside expired job leases. |
| Bounded store calls | Runtime-driven store operations (claim, heartbeat, complete, sweep, scheduler tick) wrap with `Config.StoreTimeout` (default 30s) so a pathological DB cannot hang a worker indefinitely. |
| Three concurrency layers | Per-worker cap, per-queue cap, global per-kind cap (`SetKindLimit`). |
| Cron scheduling | `Schedule(name, expr, job, opts)` with catch-up policy. Safe to run schedulers on multiple processes; an optimistic UPDATE picks the winner. |
| Progress reporting | `ctx.Progress(done, total, msg)` coalesced into one store write per 500ms, final value always flushed. |
| Pluggable storage | Memory (tests), SQLite (single-node), PostgreSQL (production, `SELECT FOR UPDATE SKIP LOCKED`). |
| Inspection API | `ListJobs` with cursor pagination, `GetJob`, `ListJobAttempts`, `ListJobSteps`, `ListQueues`, `ListWorkers`. |
| Hooks | `OnEnqueue` / `OnStart` / `OnFinish` for logging, metrics, tracing — without the core depending on any. |

## Install

```bash
go get github.com/moostackhq/go/jobs
```

## Quickstart

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/sqlite"
)

type SendEmail struct {
	UserID int64 `json:"user_id"`
}

func (j *SendEmail) Run(ctx jobs.Context) error {
	_, err := jobs.Step(ctx, "send", func(ctx context.Context) (any, error) {
		// ... actually send the email ...
		return nil, nil
	})
	return err
}

func main() {
	db, _ := sqlite.Open("jobs.db")
	defer db.Close()
	store, _ := sqlite.New(db, sqlite.Options{AutoCreate: true})

	mgr, _ := jobs.New(store, jobs.Config{Logger: slog.Default()})
	_ = jobs.Register[SendEmail](mgr, "send_email")

	// Enqueue from anywhere that holds mgr.
	mgr.Enqueue(context.Background(), &SendEmail{UserID: 42})

	worker, _ := jobs.NewWorker(mgr, jobs.WorkerConfig{Concurrency: 4})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := worker.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		panic(err)
	}
	worker.Stop(context.Background())
}
```

See `example/emails/` for an HTTP-driven end-to-end demo with hooks, scheduling, and graceful shutdown.

## Job definitions

A job is a struct with a `Run(jobs.Context) error` method on a pointer receiver. Register it with the kind name you want to persist:

```go
type ImportCSV struct {
	FileID int64 `json:"file_id"`
}

func (j *ImportCSV) Run(ctx jobs.Context) error { ... }

jobs.Register[ImportCSV](mgr, "import_csv.v1")
```

Versioning is by name: register the new struct under `import_csv.v2` and the old one under `import_csv.v1` during a rollout. Workers that pull a job whose kind is no longer registered park it as failed with `ErrUnregistered` (no retries, no queue blocking).

## Enqueue

```go
id, err := mgr.Enqueue(ctx, &ImportCSV{FileID: 42}, jobs.Options{
    Queue:       "imports",
    Priority:    5,
    MaxAttempts: 3,
    Delay:       10 * time.Second,
    Timeout:     5 * time.Minute,
    UniqueKey:   "import:42",
    Backoff:     jobs.ExponentialBackoff{Base: 30 * time.Second, Max: 10 * time.Minute, Jitter: 0.2},
})
```

`UniqueKey` collisions return `(existingID, *jobs.DuplicateError)`; `errors.Is(err, jobs.ErrDuplicate)` matches.

`Options.Backoff` is persisted as a spec on the row (today only `ExponentialBackoff` round-trips; custom `Backoff` types fall back to the manager default on retry). If unset, retry timing uses `Config.DefaultBackoff`.

`EnqueueTx(ctx, tx, job, opts)` writes the job inside a `*sql.Tx`. Memory store returns `jobs.ErrUnsupported`.

## Durable steps

```go
func (j *ImportCSV) Run(ctx jobs.Context) error {
    file, err := jobs.Step(ctx, "download", func(ctx context.Context) (File, error) {
        return downloadFile(j.FileID)
    })
    if err != nil { return err }

    rows, err := jobs.Step(ctx, "parse", func(ctx context.Context) ([]Row, error) {
        return parseCSV(file)
    })
    if err != nil { return err }

    _, err = jobs.Step(ctx, "import", func(ctx context.Context) (any, error) {
        return nil, importRows(rows)
    })
    return err
}
```

After a crash, retries pick up at the first step whose result has not been committed. Step names must be stable and deterministic; do not put loop indices in them.

Void steps (no result to pass along) use `T = any` and return `(nil, err)`. The two-character ceremony is the language tax for not having implicit unit types; the persistence semantics are identical to value-returning steps.

## Storage backends

Each backend ships its DDL as an embedded string. Construct with `AutoCreate: true` to apply it on `New`, or fetch it via `Schema()` and run it yourself.

### SQLite

```go
ddl := sqlite.Schema()
db, _ := sqlite.Open("jobs.db") // sets WAL, busy_timeout, txlock=immediate
store, _ := sqlite.New(db, sqlite.Options{AutoCreate: true})
```

Single-node only. Concurrent worker throughput is bounded by single-writer serialisation.

### PostgreSQL

```go
ddl := postgres.Schema()
db, _ := sql.Open("postgres", "postgres://...")
store, _ := postgres.New(db, postgres.Options{AutoCreate: true})
```

Distributed workers via `SELECT FOR UPDATE SKIP LOCKED`. The production backend.

### Memory

```go
store := memory.New()
```

Non-durable, for tests and demos. `EnqueueTx` returns `ErrUnsupported`.

## Workers

```go
worker, _ := jobs.NewWorker(mgr, jobs.WorkerConfig{
    Queues:            []string{"default", "emails"},
    Concurrency:       10,
    PerQueue:          map[string]int{"emails": 3},
    PollInterval:      1 * time.Second,
    LeaseDuration:     60 * time.Second,
    HeartbeatInterval: 20 * time.Second,
    SweepInterval:     30 * time.Second,
})
```

`Worker.Start(ctx)` **blocks** until ctx cancel or `Stop`. `Stop(ctx)` drains in-flight jobs until ctx expires, then cancels their contexts. Jobs that ignore cancellation outrun shutdown; the lease expires naturally and another worker reclaims them.

### Reliability defaults

Every `SweepInterval` (default 30s) the worker performs three pieces of housekeeping in addition to its claim loop:

1. **Reclaim expired job leases** (crashed-worker recovery). Each reclaimed job gets a synthetic `Attempt{State: failed, Error: "lease expired"}` row so the history reflects the crash; the `attempt` counter is bumped so the retry is correctly numbered.
2. **Reap stale worker rows** (workers with `LastSeenAt` older than 5×LeaseDuration). Removes ghost rows from `ListWorkers` when a worker dies via `kill -9` / OOM and never called `Stop`.
3. **Refresh own `LastSeenAt`** so `ListWorkers` shows this worker as alive.

Runtime-driven store calls (claim, heartbeat, complete, sweep, scheduler tick) wrap with `Config.StoreTimeout` (default 30s). A pathological backend returns `context.DeadlineExceeded` to the runtime loop instead of hanging the worker forever. Set to a negative value to disable. User-facing methods (`Enqueue`, `GetJob`, etc.) pass the caller's context unchanged.

## Scheduling

```go
mgr.Schedule(ctx, "daily-report", "0 6 * * *", &DailyReport{}, jobs.ScheduleOptions{
    Singleton: true,
    CatchUp:   jobs.CatchUpOnce,
})

go mgr.StartScheduler(ctx)
```

`Singleton: true` is sugar for `UniqueKey = "schedule:<name>"`. Multiple processes may run `StartScheduler` concurrently; an optimistic UPDATE picks the winner on each tick.

| `CatchUp` | Behaviour |
|---|---|
| `CatchUpOnce` (default) | Fire one job if any tick was missed; resume cadence |
| `CatchUpSkip` | Never fire missed ticks |
| `CatchUpAll` | Fire one job per missed tick, **capped at 1000 per firing**. A scheduler offline for a year with a 1-minute cron emits 1000 jobs + a logged warning instead of 525,600 jobs, and resumes regular cadence |

## Operations API

```go
mgr.RetryJob(ctx, id)                       // failed/discarded/cancelled → available
immediate, err := mgr.CancelJob(ctx, id)    // scheduled/available → immediate=true; running → false
mgr.DeleteJob(ctx, id)                      // refuses running; cascade-deletes attempts and steps
mgr.SetKindLimit("send_email", 5)           // global per-kind cap
```

`CancelJob` returns `immediate=true` when the job transitioned to `cancelled` synchronously (scheduled/available paths). For a running job, `immediate=false` means a flag was set; the worker observes it on the next heartbeat (within `HeartbeatInterval`), cancels the job's context, and writes the cancelled outcome on Run return. UIs can render "cancelling..." until a subsequent `GetJob` confirms the terminal transition.

Edge case: if `Run` returns successfully *before* the heartbeat observed the cancel flag, `Store.Complete` checks the flag in the same transaction as the state write and rewrites `StateSucceeded` to `StateCancelled`. The user-visible state matches the request; side effects of the already-completed Run cannot be undone.

## Hooks

All three hooks share the same shape: `func(ctx context.Context, e EventStruct)`.

```go
jobs.Config{
    Hooks: jobs.Hooks{
        OnEnqueue: func(ctx context.Context, e jobs.EnqueueEvent) { ... },
        OnStart:   func(ctx context.Context, e jobs.StartEvent)   { ... },
        OnFinish:  func(ctx context.Context, e jobs.FinishEvent)  { ... },
    },
}
```

| Event | Fields |
|---|---|
| `EnqueueEvent` | `JobID`, `Kind`, `Job`, `Opts`, `Tx`, `ScheduleName` (non-empty when the enqueue came from `StartScheduler`) |
| `StartEvent` | `JobID`, `Kind`, `Attempt`, `Logger` |
| `FinishEvent` | embeds `StartEvent`; adds `Err`, `Dur` |

`OnFinish` fires via `defer` so it always emits, including for panic-converted errors and context cancellations.

## Schema management

The DDL is idempotent (every statement uses `IF NOT EXISTS`), so applying it on every process start is safe even when you also manage schema externally:

```go
ddl := sqlite.Schema()    // or postgres.Schema()
```

The jobs package does not depend on any external migration library. If the schema ever needs a v2, that is the moment to introduce migrations, not before.
