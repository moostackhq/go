package jobs

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"
)

// Config controls manager-wide defaults. The zero value is unusable;
// callers must at minimum supply a [Store] via [New]. The schema-
// creation knob lives on each backend's own Options (for example,
// sqlite.Options{AutoCreate: true}), not here.
type Config struct {
	// Logger is wired through to [Context.Logger]. Defaults to
	// slog.Default() when nil.
	Logger *slog.Logger
	// DefaultQueue is the queue assigned when [Options.Queue] is "".
	// Defaults to "default".
	DefaultQueue string
	// DefaultBackoff is used when [Options.Backoff] is nil. Defaults
	// to [DefaultBackoff].
	DefaultBackoff Backoff
	// DefaultMaxAttempts is used when [Options.MaxAttempts] is 0.
	// Defaults to 25.
	DefaultMaxAttempts int
	// Hooks is wired through to enqueue, run, and finish callbacks.
	Hooks Hooks
	// SchedulerInterval is the tick cadence used by
	// [Manager.StartScheduler]. Defaults to 1s. Shorter values are
	// useful in tests; production values rarely need to change.
	// Clamped to [MaxSchedulerInterval] (60s) with a logged
	// warning: cron's smallest standard resolution is one minute,
	// so any tick cadence beyond that would routinely miss
	// schedules.
	SchedulerInterval time.Duration
	// StoreTimeout caps runtime-driven store operations (claim,
	// heartbeat, complete, sweep, progress flush, attempt record,
	// scheduler tick). A pathological DB would otherwise hang a
	// worker indefinitely. Defaults to 30s; set to a negative
	// value to disable. User-facing methods (Enqueue, GetJob,
	// ListJobs, RetryJob, etc.) pass the caller's context
	// unchanged and are unaffected.
	StoreTimeout time.Duration
}

// Manager is the entry point: register types, enqueue work, inspect
// state, drive operator actions. Workers (see [Worker]) are
// constructed separately and bound to a manager.
type Manager struct {
	store  Store
	config Config

	mu                sync.RWMutex
	constructorByName map[string]func() Job
	nameByType        map[reflect.Type]string

	limitsMu   sync.RWMutex
	kindLimits map[string]int
}

// New constructs a manager bound to a store.
func New(s Store, cfg Config) (*Manager, error) {
	if s == nil {
		return nil, fmt.Errorf("jobs.New: store is nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DefaultQueue == "" {
		cfg.DefaultQueue = "default"
	}
	if cfg.DefaultBackoff == nil {
		cfg.DefaultBackoff = DefaultBackoff
	}
	if cfg.DefaultMaxAttempts < 0 {
		return nil, fmt.Errorf("jobs.New: DefaultMaxAttempts must be >= 0 (got %d; use 0 for the built-in default of 25, or a positive cap)", cfg.DefaultMaxAttempts)
	}
	if cfg.DefaultMaxAttempts == 0 {
		cfg.DefaultMaxAttempts = 25
	}
	if cfg.StoreTimeout == 0 {
		cfg.StoreTimeout = 30 * time.Second
	} else if cfg.StoreTimeout < 0 {
		cfg.StoreTimeout = 0 // explicit "no timeout"
	}
	if cfg.SchedulerInterval > MaxSchedulerInterval {
		cfg.Logger.Warn("jobs.New: SchedulerInterval too large, clamping",
			"requested", cfg.SchedulerInterval,
			"max", MaxSchedulerInterval)
		cfg.SchedulerInterval = MaxSchedulerInterval
	}
	return &Manager{
		store:             s,
		config:            cfg,
		constructorByName: make(map[string]func() Job),
		nameByType:        make(map[reflect.Type]string),
		kindLimits:        make(map[string]int),
	}, nil
}

// safeHook invokes fn with a deferred recover so a panicking
// callback does not crash the calling goroutine (the worker for
// OnStart / OnFinish, the enqueueing caller for OnEnqueue).
// Panics are logged at error level via [Config.Logger]; everything
// else continues normally.
func (m *Manager) safeHook(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			m.config.Logger.Error("jobs: hook panic recovered",
				"hook", name, "panic", r)
		}
	}()
	fn()
}

// withStoreTimeout wraps parent with [Config.StoreTimeout] so a
// hung backend cannot stall a worker forever. Returns parent
// unchanged when the timeout is disabled (negative config value).
// Caller must always call the returned CancelFunc.
//
// Used by every runtime-driven store call. User-facing methods
// (Enqueue, GetJob, ListJobs, etc.) pass the caller's context
// straight to the store without wrapping.
func (m *Manager) withStoreTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if m.config.StoreTimeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, m.config.StoreTimeout)
}

// SetKindLimit installs (or removes, when limit <= 0) a global cap
// on the number of jobs of the given kind that may run concurrently
// across the cluster. The limit is enforced inside [Store.Claim].
// Safe to call at any time.
func (m *Manager) SetKindLimit(kind string, limit int) {
	m.limitsMu.Lock()
	defer m.limitsMu.Unlock()
	if limit <= 0 {
		delete(m.kindLimits, kind)
		return
	}
	m.kindLimits[kind] = limit
}

// snapshotKindLimits returns a shallow copy of the current limits
// map for use in [ClaimRequest]. Worker calls this on every poll
// tick, so the value reflects the latest SetKindLimit calls without
// holding the lock during the claim.
func (m *Manager) snapshotKindLimits() map[string]int {
	m.limitsMu.RLock()
	defer m.limitsMu.RUnlock()
	if len(m.kindLimits) == 0 {
		return nil
	}
	out := make(map[string]int, len(m.kindLimits))
	for k, v := range m.kindLimits {
		out[k] = v
	}
	return out
}

// Register binds a kind name to its Go type. The PT constraint
// `interface{ *T; Job }` encodes "pointer-to-T satisfies Job," which
// lets callers write Register[SendEmail] (Go infers PT = *SendEmail)
// instead of the noisier Register[*SendEmail].
//
// The manager stores a constructor that returns a fresh *T on every
// dispatch, plus a type-to-name index used by [Manager.Enqueue] to
// look up the kind without callers having to repeat it.
//
// Returns [ErrKindAlreadyRegistered] if name is already bound.
func Register[T any, PT interface {
	*T
	Job
}](m *Manager, name string) error {
	if m == nil {
		return fmt.Errorf("jobs.Register: nil manager")
	}
	if name == "" {
		return fmt.Errorf("jobs.Register: empty name")
	}

	constructor := func() Job { return PT(new(T)) }
	typ := reflect.TypeOf(constructor())

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.constructorByName[name]; exists {
		return ErrKindAlreadyRegistered
	}
	if existing, exists := m.nameByType[typ]; exists {
		return fmt.Errorf("type %v already registered as %q", typ, existing)
	}
	m.constructorByName[name] = constructor
	m.nameByType[typ] = name
	return nil
}

// Enqueue persists a new job. opts is variadic for ergonomics: most
// call sites do not need it, and Go has no way to express a default
// argument. Returns the new job's id.
//
// On UniqueKey collision, returns the existing job's id together
// with a [*DuplicateError]; callers that do not care can ignore the
// error, callers that do can errors.Is / errors.As it.
func (m *Manager) Enqueue(ctx context.Context, job Job, opts ...Options) (string, error) {
	return m.enqueue(ctx, nil, "", job, opts)
}

// EnqueueTx writes the job inside the caller's transaction. The job
// is not visible to workers until the transaction commits. Memory
// stores return [ErrUnsupported].
func (m *Manager) EnqueueTx(ctx context.Context, tx *sql.Tx, job Job, opts ...Options) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("jobs.EnqueueTx: nil tx")
	}
	return m.enqueue(ctx, tx, "", job, opts)
}

// enqueue is the shared implementation. scheduleName is non-empty
// when the call originated from [Manager.StartScheduler]; the value
// is surfaced via [EnqueueEvent.ScheduleName] so hooks can tell
// scheduler-driven enqueues apart from caller-driven ones.
func (m *Manager) enqueue(ctx context.Context, tx *sql.Tx, scheduleName string, job Job, opts []Options) (string, error) {
	if job == nil {
		return "", fmt.Errorf("jobs.Enqueue: nil job")
	}
	typ := reflect.TypeOf(job)
	m.mu.RLock()
	name, ok := m.nameByType[typ]
	var ptrRegisteredAs string
	if !ok && typ.Kind() != reflect.Pointer {
		// Common mistake: user passed the value type when Register
		// requires a pointer (the PT constraint on Register
		// guarantees the registered type is always *T). Look the
		// pointer-to type up so we can emit a hinting error
		// instead of the bare "unregistered" message.
		ptrRegisteredAs = m.nameByType[reflect.PointerTo(typ)]
	}
	m.mu.RUnlock()
	if !ok {
		if ptrRegisteredAs != "" {
			return "", fmt.Errorf("jobs.Enqueue: pass a pointer (got value type %v, registered as %q under *%v; use &%v{...})",
				typ, ptrRegisteredAs, typ, typ)
		}
		return "", fmt.Errorf("%w: %v", ErrUnregistered, typ)
	}

	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	m.applyDefaults(&o)
	if o.MaxAttempts < 0 {
		return "", fmt.Errorf("jobs.Enqueue: MaxAttempts must be >= 0 (got %d; use 0 for the manager default, or a positive cap)", o.MaxAttempts)
	}

	payload, err := encodePayload(job)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	row := &JobRow{
		ID:           NewID(),
		Kind:         name,
		Payload:      payload,
		Queue:        o.Queue,
		Priority:     o.Priority,
		Attempt:      0,
		MaxAttempts:  o.MaxAttempts,
		AvailableAt:  runAt(o, now),
		TimeoutMs:    int64(o.Timeout / time.Millisecond),
		OnTimeoutInt: int(o.OnTimeout),
		BackoffSpec:  encodeBackoff(o.Backoff),
		UniqueKey:    o.UniqueKey,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if row.AvailableAt.After(now) {
		row.State = StateScheduled
	} else {
		row.State = StateAvailable
	}

	if tx != nil {
		err = m.store.InsertTx(ctx, tx, row)
	} else {
		err = m.store.Insert(ctx, row)
	}
	if err != nil {
		var dup *DuplicateError
		if errors.As(err, &dup) {
			return dup.ExistingID, err
		}
		return "", err
	}

	if m.config.Hooks.OnEnqueue != nil {
		// Fires after a successful Insert/InsertTx so observers do
		// not see phantom events from failed inserts. For EnqueueTx
		// this is still pre-commit: the caller's transaction may
		// later roll back, in which case the row never becomes
		// visible to workers even though the hook fired. Observers
		// that need commit-accurate semantics should hook into their
		// own tx commit instead. See [EnqueueEvent.Tx].
		//
		// Hooks see the EFFECTIVE backoff (manager default when the
		// user did not override), not the persisted backoff_spec.
		// This is a copy: persistence (row.BackoffSpec) still
		// reflects only the user-supplied override so the manager
		// default isn't baked into the row.
		hookOpts := o
		if hookOpts.Backoff == nil {
			hookOpts.Backoff = m.config.DefaultBackoff
		}
		m.safeHook("OnEnqueue", func() {
			m.config.Hooks.OnEnqueue(ctx, EnqueueEvent{
				JobID:        row.ID,
				Kind:         name,
				Job:          job,
				Opts:         hookOpts,
				Tx:           tx != nil,
				ScheduleName: scheduleName,
			})
		})
	}
	return row.ID, nil
}

func (m *Manager) applyDefaults(o *Options) {
	if o.Queue == "" {
		o.Queue = m.config.DefaultQueue
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = m.config.DefaultMaxAttempts
	}
	// Intentionally do NOT copy m.config.DefaultBackoff into
	// o.Backoff: that would persist the default into every row's
	// backoff_spec, and the runner's backoffFor() already falls
	// back to the manager default when the row has no override.
}

// runAt resolves Delay and RunAt into the wall-clock time the job
// first becomes claimable. RunAt wins when both are set.
func runAt(o Options, now time.Time) time.Time {
	if !o.RunAt.IsZero() {
		return o.RunAt.UTC()
	}
	if o.Delay > 0 {
		return now.Add(o.Delay)
	}
	return now
}

// GetJob returns the inspection view of a single job.
// Returns [ErrNotFound] when no such job exists.
func (m *Manager) GetJob(ctx context.Context, id string) (*JobInfo, error) {
	row, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	info := rowToInfo(row)
	return &info, nil
}

// ListJobs returns a page of jobs matching the filter, with an
// opaque cursor for the next page (empty when exhausted).
func (m *Manager) ListJobs(ctx context.Context, f JobFilter) (JobPage, error) {
	rows, next, err := m.store.List(ctx, f)
	if err != nil {
		return JobPage{}, err
	}
	out := JobPage{
		Jobs:       make([]JobInfo, 0, len(rows)),
		NextCursor: next,
	}
	for _, r := range rows {
		out.Jobs = append(out.Jobs, rowToInfo(r))
	}
	return out, nil
}

func rowToInfo(r *JobRow) JobInfo {
	return JobInfo{
		ID:          r.ID,
		Kind:        r.Kind,
		Queue:       r.Queue,
		Priority:    r.Priority,
		State:       r.State,
		Attempt:     r.Attempt,
		MaxAttempts: r.MaxAttempts,
		AvailableAt: r.AvailableAt,
		Timeout:     time.Duration(r.TimeoutMs) * time.Millisecond,
		UniqueKey:   r.UniqueKey,
		Payload:     append([]byte(nil), r.Payload...),
		Progress: Progress{
			Done:  r.ProgressDone,
			Total: r.ProgressTotal,
			Msg:   r.ProgressMsg,
		},
		Error:           r.Error,
		CancelRequested: r.CancelRequested,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

// NewID returns a UUIDv4 string. Public so backend implementors
// (and the included memory / SQL stores) can generate IDs for
// attempt / step rows without pulling in a third-party UUID
// dependency.
func NewID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on a healthy system; treat it
		// like out-of-memory: fatal and immediate.
		panic("jobs: crypto/rand.Read failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
