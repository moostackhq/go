package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduleOptions controls a scheduled enqueue. Fields are a subset
// of [Options]: Backoff is intentionally omitted because [Backoff]
// is an interface and cannot be serialized into the schedule row;
// scheduled enqueues use the manager's DefaultBackoff.
type ScheduleOptions struct {
	Queue       string        `json:"queue,omitempty"`
	Priority    int           `json:"priority,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
	OnTimeout   OnTimeout     `json:"on_timeout,omitempty"`
	// Singleton: when true, enqueued jobs carry
	// UniqueKey="schedule:<name>" so a previous still-running fire
	// blocks the new one.
	Singleton bool `json:"singleton,omitempty"`
	// CatchUp decides how missed ticks are handled. Default is
	// [CatchUpOnce].
	CatchUp CatchUp `json:"catch_up,omitempty"`
}

// Schedule registers (or upserts) a recurring job. The kind must
// already be registered with [Register]. cronExpr is parsed with
// the standard 5-field cron grammar.
//
// Calling Schedule with the same name on every boot is safe: the
// row is upserted, so duplicates do not accumulate.
//
// Renaming a registered type breaks existing schedules silently:
// the schedule row stores the kind name resolved at Schedule time,
// so if a later boot registers the same Go type under a different
// name, scheduler ticks log "kind not registered" and park. Either
// keep the old name registered alongside the new one, or call
// [Manager.Unschedule] + [Manager.Schedule] after the rename.
func (m *Manager) Schedule(ctx context.Context, name, cronExpr string, job Job, opts ScheduleOptions) error {
	if name == "" {
		return fmt.Errorf("jobs.Schedule: empty name")
	}
	if job == nil {
		return fmt.Errorf("jobs.Schedule: nil job")
	}
	parsed, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return fmt.Errorf("jobs.Schedule: parse cron: %w", err)
	}

	typ := reflect.TypeOf(job)
	m.mu.RLock()
	kind, ok := m.nameByType[typ]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %v", ErrUnregistered, typ)
	}

	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("jobs.Schedule: encode payload: %w", err)
	}
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("jobs.Schedule: encode options: %w", err)
	}

	now := time.Now().UTC()
	return m.store.UpsertSchedule(ctx, &ScheduleRow{
		Name:        name,
		Kind:        kind,
		Cron:        cronExpr,
		Payload:     payload,
		OptionsJSON: optsJSON,
		NextRunAt:   parsed.Next(now),
		UpdatedAt:   now,
	})
}

// Unschedule removes a schedule by name. Calling Unschedule for a
// name that does not exist is not an error.
func (m *Manager) Unschedule(ctx context.Context, name string) error {
	return m.store.DeleteSchedule(ctx, name)
}

// ListSchedules returns every persisted schedule, ordered by name.
// Rows with corrupt OptionsJSON come back with a zero-value
// [ScheduleOptions] and a logged warning; the rest of the row is
// preserved so the caller still sees the schedule's name, kind,
// and cron expression.
func (m *Manager) ListSchedules(ctx context.Context) ([]ScheduleInfo, error) {
	rows, err := m.store.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleInfo, len(rows))
	for i, r := range rows {
		var opts ScheduleOptions
		if err := json.Unmarshal(r.OptionsJSON, &opts); err != nil {
			m.config.Logger.Warn("jobs: ListSchedules: bad options JSON",
				"schedule", r.Name, "err", err)
		}
		out[i] = ScheduleInfo{
			Name:      r.Name,
			Kind:      r.Kind,
			Cron:      r.Cron,
			Options:   opts,
			NextRunAt: r.NextRunAt,
			LastRunAt: r.LastRunAt,
		}
	}
	return out, nil
}

// ScheduleInfo is the inspection-time view of a schedule row.
type ScheduleInfo struct {
	Name      string
	Kind      string
	Cron      string
	Options   ScheduleOptions
	NextRunAt time.Time
	LastRunAt time.Time
}

// StartScheduler blocks, ticking once per [Config.SchedulerInterval]
// (default 1s), enqueueing any due schedules per their CatchUp
// policy. Multiple processes may call StartScheduler concurrently;
// the optimistic [Store.ClaimSchedule] ensures each tick enqueues at
// most once per schedule.
//
// Returns ctx.Err() when ctx is cancelled.
func (m *Manager) StartScheduler(ctx context.Context) error {
	interval := m.config.SchedulerInterval
	if interval <= 0 {
		interval = 1 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.schedulerTick(ctx)
		}
	}
}

func (m *Manager) schedulerTick(ctx context.Context) {
	now := time.Now().UTC()
	dueCtx, dueCancel := m.withStoreTimeout(ctx)
	due, err := m.store.DueSchedules(dueCtx, now)
	dueCancel()
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler: DueSchedules failed", "err", err)
		return
	}
	for _, row := range due {
		m.fireSchedule(ctx, row, now)
	}
}

func (m *Manager) fireSchedule(ctx context.Context, row *ScheduleRow, now time.Time) {
	parsed, err := cron.ParseStandard(row.Cron)
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler: bad cron expr",
			"schedule", row.Name, "err", err)
		return
	}
	var opts ScheduleOptions
	if err := json.Unmarshal(row.OptionsJSON, &opts); err != nil {
		m.config.Logger.Warn("jobs: scheduler: bad options",
			"schedule", row.Name, "err", err)
		return
	}

	// Compute number of fires plus the next future run time, based on
	// CatchUp policy.
	enqueues, nextRun, capped := planFires(parsed, row.NextRunAt, now, opts.CatchUp)
	if capped {
		m.config.Logger.Warn("jobs: scheduler: CatchUpAll capped",
			"schedule", row.Name,
			"fires", enqueues,
			"cap", catchUpAllMax,
			"next_run_at", nextRun,
		)
	}
	if enqueues == 0 {
		// Nothing to enqueue, but still advance NextRunAt so we do
		// not keep waking up on this schedule every tick.
		advCtx, advCancel := m.withStoreTimeout(ctx)
		_, _ = m.store.ClaimSchedule(advCtx, row.Name, row.LastRunAt, row.LastRunAt, nextRun)
		advCancel()
		return
	}

	// Optimistic claim: only one scheduler advances the row.
	claimCtx, claimCancel := m.withStoreTimeout(ctx)
	claimed, err := m.store.ClaimSchedule(claimCtx, row.Name, row.LastRunAt, now, nextRun)
	claimCancel()
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler: claim failed",
			"schedule", row.Name, "err", err)
		return
	}
	if !claimed {
		return
	}

	options := Options{
		Queue:       opts.Queue,
		Priority:    opts.Priority,
		MaxAttempts: opts.MaxAttempts,
		Timeout:     opts.Timeout,
		OnTimeout:   opts.OnTimeout,
	}
	if opts.Singleton {
		options.UniqueKey = "schedule:" + row.Name
	}

	m.mu.RLock()
	constructor, ok := m.constructorByName[row.Kind]
	m.mu.RUnlock()
	if !ok {
		m.config.Logger.Warn("jobs: scheduler: kind not registered",
			"schedule", row.Name, "kind", row.Kind)
		return
	}

	for i := 0; i < enqueues; i++ {
		job := constructor()
		if err := json.Unmarshal(row.Payload, job); err != nil {
			m.config.Logger.Warn("jobs: scheduler: decode payload",
				"schedule", row.Name, "err", err)
			return
		}
		// Use the internal enqueue path directly so we can attach
		// the schedule name to EnqueueEvent. The public Enqueue
		// passes "" for scheduleName.
		_, err := m.enqueue(ctx, nil, row.Name, job, []Options{options})
		if err != nil && !errors.Is(err, ErrDuplicate) {
			m.config.Logger.Warn("jobs: scheduler: enqueue",
				"schedule", row.Name, "err", err)
		}
	}
}

// MaxSchedulerInterval caps [Config.SchedulerInterval]. Cron's
// smallest standard resolution is one minute, so a tick cadence
// beyond that would routinely miss schedules; [Manager.New]
// clamps anything larger and logs a warning.
const MaxSchedulerInterval = 60 * time.Second

// catchUpAllMax caps the number of enqueues a single CatchUpAll
// firing can produce. A scheduler offline for a year with a
// 1-minute cron would otherwise emit 525,600 jobs on first tick;
// the cap turns that into a logged warning + a manageable batch.
// Set deliberately high enough that normal downtime windows
// (hours / days for most cadences) stay within bounds.
const catchUpAllMax = 1000

// planFires returns how many enqueues this tick should produce,
// the new NextRunAt to persist, and whether the [CatchUpAll] cap
// was hit (capped == true means the caller should log a warning;
// the engine still advances NextRunAt past `now` so subsequent
// ticks do not keep firing on the same backlog).
func planFires(parsed cron.Schedule, nextRun, now time.Time, catchUp CatchUp) (count int, next time.Time, capped bool) {
	// Advance up to (but not past) now, accumulating per policy.
	next = nextRun
	switch catchUp {
	case CatchUpSkip:
		// Skip every missed tick. Jump straight to the next future
		// tick; iterating one Next per missed tick is O(backlog)
		// for no work product.
		if !next.After(now) {
			next = parsed.Next(now)
		}
		return 0, next, false
	case CatchUpAll:
		for !next.After(now) {
			if count >= catchUpAllMax {
				// Hit the cap. Same O(1) jump as the Skip path
				// for the remaining backlog.
				next = parsed.Next(now)
				return count, next, true
			}
			count++
			next = parsed.Next(next)
		}
		return count, next, false
	default: // CatchUpOnce
		if next.After(now) {
			return 0, next, false
		}
		// One fire, then jump past the entire backlog in O(1).
		return 1, parsed.Next(now), false
	}
}
