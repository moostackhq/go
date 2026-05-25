package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// defaultMaxRetries bounds [Manager.Update]'s CAS retry loop. Three is
// enough to absorb realistic concurrent-tab contention without masking
// pathological hot-spotting on a single session.
const defaultMaxRetries = 3

// Config configures a [Manager]. Store and Token are required; expiry
// durations must be positive. All other fields are zero-value safe.
//
// The session payload codec lives on the [Store] (each store knows how
// to talk to its backing medium), not here.
type Config[T any] struct {
	Store          Store[T]
	Token          Token
	AbsoluteExpiry time.Duration
	IdleExpiry     time.Duration

	// IdleBumpInterval is the minimum gap between sliding-expiry
	// extensions for sessions that were read but not mutated during
	// a request. Zero disables bumping (clean reads never write).
	// Must not exceed IdleExpiry. Bumping requires the store to
	// implement [TTLBumper]; against other stores the field is
	// honoured but the bump is a no-op.
	IdleBumpInterval time.Duration

	// MaxRetries is the maximum number of CAS retries
	// [Manager.Update] performs on top of the initial attempt —
	// so MaxRetries=N permits at most N+1 closure invocations.
	// Zero defaults to the package default (3). Negative is
	// clamped to 0, which means a single attempt with no retries.
	MaxRetries int

	// Now is the manager's wall-clock source. Tests inject a
	// controllable clock; production leaves it nil to use time.Now.
	Now func() time.Time

	// NewSID overrides the session ID generator. Tests use this to
	// produce deterministic IDs; production leaves it nil.
	NewSID func() (string, error)

	// Cloner, if set, produces an isolated copy of the payload
	// that [Manager.Update] passes to its closure. It is called
	// once per attempt — so the closure can mutate maps, slices,
	// or pointer-fields without leaking those mutations back into
	// the per-request state on closure error or terminal commit
	// failure. Leave nil for value-only payload types (no maps,
	// no slices accessed by index, no pointers); the default
	// shallow copy is sufficient there.
	Cloner func(T) T
}

// Manager is the request-scoped session controller. Construct one per
// payload type T at process start and reuse it across requests; the
// manager is safe for concurrent use.
type Manager[T any] struct {
	cfg        Config[T]
	now        func() time.Time
	newSID     func() (string, error)
	clone      func(T) T
	maxRetries int
	ctxKey     *managerKey[T]
}

// managerKey is a per-Manager pointer used as a context key. Two
// managers of the same T do not collide because each one allocates its
// own *managerKey[T].
type managerKey[T any] struct{}

// New returns a configured [Manager]. It returns an error rather
// than panicking on a misconfigured Config so callers can surface
// the failure in process startup. All validation errors are
// collected and joined via [errors.Join] so a caller misconfiguring
// several fields sees every problem in one pass.
func New[T any](cfg Config[T]) (*Manager[T], error) {
	var problems []error
	if cfg.Store == nil {
		problems = append(problems, errors.New("session.New: Store is required"))
	}
	if cfg.Token == nil {
		problems = append(problems, errors.New("session.New: Token is required"))
	}
	if cfg.AbsoluteExpiry <= 0 {
		problems = append(problems, errors.New("session.New: AbsoluteExpiry must be > 0"))
	}
	if cfg.IdleExpiry <= 0 {
		problems = append(problems, errors.New("session.New: IdleExpiry must be > 0"))
	}
	if cfg.AbsoluteExpiry > 0 && cfg.IdleExpiry > 0 && cfg.IdleExpiry > cfg.AbsoluteExpiry {
		problems = append(problems, errors.New("session.New: IdleExpiry must not exceed AbsoluteExpiry"))
	}
	if cfg.IdleBumpInterval < 0 || (cfg.IdleExpiry > 0 && cfg.IdleBumpInterval > cfg.IdleExpiry) {
		problems = append(problems, errors.New("session.New: IdleBumpInterval must be in [0, IdleExpiry]"))
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	m := &Manager[T]{
		cfg:        cfg,
		now:        cfg.Now,
		newSID:     cfg.NewSID,
		clone:      cfg.Cloner,
		maxRetries: cfg.MaxRetries,
		ctxKey:     &managerKey[T]{},
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.newSID == nil {
		m.newSID = generateSID
	}
	switch {
	case m.maxRetries < 0:
		m.maxRetries = 0
	case m.maxRetries == 0:
		m.maxRetries = defaultMaxRetries
	}
	return m, nil
}

// state is the per-request session bookkeeping the middleware attaches
// to the context. Get/Save/Update/Destroy/Renew/Promote/SID operate on
// this struct; the middleware drains it at end-of-request.
//
// origSID is the SID the client sent on the inbound request, captured
// once by the middleware and never modified after. sid is the current
// SID the server is tracking — it can diverge from origSID when a new
// session is minted, a stale cookie is dropped on load failure, or
// [Manager.Renew] rotates. Cookie emission at commit time is derived
// from sid != origSID, so the same rule covers all three cases.
type state[T any] struct {
	origSID string
	sid     string
	record  Record[T]
	loaded  bool
	payload T
	dirty   bool
	destroy bool
	renew   bool
}

// Get returns a pointer into the per-request session payload. The
// first call triggers Store.Load; subsequent calls return the same
// pointer so mutations are visible to later code in the same request.
//
// Get does not mark the session dirty. Mutations are not persisted
// unless the caller also calls [Manager.Save] (deferred, batched at
// end-of-request) or [Manager.Update] (atomic, immediate).
func (m *Manager[T]) Get(ctx context.Context) (*T, error) {
	st, err := m.stateFromCtx(ctx, "Get")
	if err != nil {
		return nil, err
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return nil, err
	}
	return &st.payload, nil
}

// Save marks the session dirty. The actual store write is performed
// by the middleware just before the response is committed, so
// multiple Save calls in a single request collapse into one write.
// Returns an error wrapping [ErrSessionDestroyed] if the session
// has already been Destroyed in the same request.
func (m *Manager[T]) Save(ctx context.Context) error {
	st, err := m.stateFromCtx(ctx, "Save")
	if err != nil {
		return err
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if st.destroy {
		return fmt.Errorf("session.Save: %w", ErrSessionDestroyed)
	}
	st.dirty = true
	return nil
}

// Update applies fn to a working copy of the payload and writes the
// result through the store with optimistic CAS. On ErrVersionConflict
// the closure is replayed against the freshly reloaded payload, up
// to Config.MaxRetries additional times — so MaxRetries=N permits at
// most N+1 closure invocations.
//
// The closure must be free of side effects on external systems
// (sending email, calling APIs); on retry it will run again. Callers
// that need to coordinate session writes with external effects should
// not use Update.
//
// Isolation: the working copy is a shallow Go copy by default. For
// payload types T that contain maps, slices mutated by index, or
// pointers, fn's mutations can otherwise reach the underlying
// per-request state and leak on closure error or terminal commit
// failure. Set [Config.Cloner] to take a deep copy per attempt.
func (m *Manager[T]) Update(ctx context.Context, fn func(*T) error) error {
	st, err := m.stateFromCtx(ctx, "Update")
	if err != nil {
		return err
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if st.destroy {
		return fmt.Errorf("session.Update: %w", ErrSessionDestroyed)
	}

	for attempt := 0; ; attempt++ {
		working := st.payload
		if m.clone != nil {
			working = m.clone(working)
		}
		if err := fn(&working); err != nil {
			return err
		}
		stored, err := m.commitPayload(ctx, st, working)
		if err == nil {
			st.record = stored
			st.payload = stored.Payload
			st.dirty = true
			return nil
		}
		if !errors.Is(err, ErrVersionConflict) || attempt >= m.maxRetries {
			return err
		}
		fresh, lerr := m.cfg.Store.Load(ctx, st.sid)
		if lerr != nil {
			return lerr
		}
		st.record = fresh
		st.payload = fresh.Payload
	}
}

// Destroy marks the session for deletion. The store row is removed and
// the cookie is cleared by the middleware at end-of-request.
func (m *Manager[T]) Destroy(ctx context.Context) error {
	st, err := m.stateFromCtx(ctx, "Destroy")
	if err != nil {
		return err
	}
	st.destroy = true
	st.dirty = false
	st.renew = false
	return nil
}

// Renew rotates the session ID at end-of-request, preserving the
// payload. The new ID is written before the old one is deleted, so a
// crash between the two leaves an extra stale row (which will expire
// naturally) rather than a logged-out user.
//
// Renew also marks the session dirty so any pending payload mutations
// land on the new ID. Calling Renew on a brand-new session is
// equivalent to Save.
func (m *Manager[T]) Renew(ctx context.Context) error {
	st, err := m.stateFromCtx(ctx, "Renew")
	if err != nil {
		return err
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if st.destroy {
		return fmt.Errorf("session.Renew: %w", ErrSessionDestroyed)
	}
	st.renew = true
	st.dirty = true
	return nil
}

// SID returns the session ID attached to the current request, or
// the empty string if no session has been committed yet.
//
// [Manager.Promote] and [Manager.Renew] schedule a SID rotation that
// only takes effect at commit time. SID returns the pre-rotation
// value until then — the next request will see the new SID via its
// cookie.
func (m *Manager[T]) SID(ctx context.Context) (string, error) {
	st, err := m.stateFromCtx(ctx, "SID")
	if err != nil {
		return "", err
	}
	return st.sid, nil
}

// UserID returns the userID currently attached to the session, or
// the empty string if the session is new or has no identity yet.
//
// The userID lives on the [Record], not the payload T. It is the
// canonical store-side identity used by [Manager.ListForUser] and
// [Manager.RevokeAllForUser], and the value [Manager.Promote]
// sets. Do not also embed a userID field inside T: a Promote call
// updates the Record but cannot reach into T, so the two would
// drift apart silently. Read identity through UserID; let the
// payload carry only domain data.
func (m *Manager[T]) UserID(ctx context.Context) (string, error) {
	st, err := m.stateFromCtx(ctx, "UserID")
	if err != nil {
		return "", err
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return "", err
	}
	return st.record.UserID, nil
}

// Promote attaches userID to the current session and rotates its SID
// at commit time. Use it on every privilege change — login, role
// escalation, password reset — to foreclose session-fixation: an
// attacker who controlled the pre-login SID cannot ride the
// post-login one. The new record is written with the new UserID
// before the old SID is deleted, so a crash mid-rotation leaves the
// user with a working session, not a logged-out one.
//
// Passing the same userID the session already has is allowed and
// still rotates the SID; pass the empty string to clear identity
// while rotating.
func (m *Manager[T]) Promote(ctx context.Context, userID string) error {
	st, err := m.stateFromCtx(ctx, "Promote")
	if err != nil {
		return err
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if st.destroy {
		return fmt.Errorf("session.Promote: %w", ErrSessionDestroyed)
	}
	st.record.UserID = userID
	st.renew = true
	st.dirty = true
	return nil
}

// ListForUser returns the SIDs of every live session belonging to
// userID. Returns an error wrapping [ErrCapabilityMissing] if the
// configured [Store] does not implement [UserIndexer].
func (m *Manager[T]) ListForUser(ctx context.Context, userID string) ([]string, error) {
	idx, ok := m.cfg.Store.(UserIndexer)
	if !ok {
		return nil, fmt.Errorf("session.ListForUser: %w", ErrCapabilityMissing)
	}
	return idx.ListByUser(ctx, userID)
}

// RevokeAllForUser deletes every live session belonging to userID,
// skipping any SIDs passed in except. Typical use from inside a
// request: pass mgr.SID(ctx) as the only exception so the calling
// device stays logged in while every other one is signed out.
// Returns the number of sessions revoked.
//
// Returns an error wrapping [ErrCapabilityMissing] if the configured
// [Store] does not implement [UserIndexer].
func (m *Manager[T]) RevokeAllForUser(ctx context.Context, userID string, except ...string) (int, error) {
	idx, ok := m.cfg.Store.(UserIndexer)
	if !ok {
		return 0, fmt.Errorf("session.RevokeAllForUser: %w", ErrCapabilityMissing)
	}
	return idx.RevokeByUser(ctx, userID, except...)
}

// stateFromCtx retrieves the per-request session state attached by
// [Manager.Wrap]. The op string is used to prefix the wrapped
// [ErrNoSession] so the diagnostic identifies which Manager method
// was called outside of a wrapped request.
func (m *Manager[T]) stateFromCtx(ctx context.Context, op string) (*state[T], error) {
	st, ok := ctx.Value(m.ctxKey).(*state[T])
	if !ok {
		return nil, fmt.Errorf("session.%s: %w", op, ErrNoSession)
	}
	return st, nil
}

func (m *Manager[T]) ensureLoaded(ctx context.Context, st *state[T]) error {
	if st.loaded {
		return nil
	}
	if st.sid == "" {
		st.loaded = true
		return nil
	}
	rec, err := m.cfg.Store.Load(ctx, st.sid)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Stale or forged cookie. Drop the supplied SID; the
			// commit phase will mint a fresh one rather than
			// reuse a value the store has no record of.
			st.sid = ""
			st.loaded = true
			return nil
		}
		return err
	}
	st.record = rec
	st.payload = rec.Payload
	st.loaded = true
	return nil
}

// commitPayload writes working through the store, handling new-session
// SID generation and expiry stamping. It is shared between Update
// (immediate, mid-request) and the middleware finaliser (deferred,
// end-of-request).
func (m *Manager[T]) commitPayload(ctx context.Context, st *state[T], working T) (Record[T], error) {
	now := m.now()
	rec := st.record
	rec.Payload = working
	rec.IdleExpiry = now.Add(m.cfg.IdleExpiry)

	if st.sid == "" {
		sid, err := m.newSID()
		if err != nil {
			return Record[T]{}, fmt.Errorf("session: generate sid: %w", err)
		}
		rec.SID = sid
		rec.Version = 0
		// Preserve a non-zero AbsoluteExpiry — Renew/Promote rotate
		// the SID but keep the hard deadline so renewals cannot
		// silently extend the session's maximum lifetime.
		if rec.AbsoluteExpiry.IsZero() {
			rec.AbsoluteExpiry = now.Add(m.cfg.AbsoluteExpiry)
		}
		stored, err := m.cfg.Store.Save(ctx, rec)
		if err != nil {
			return Record[T]{}, err
		}
		st.sid = sid
		return stored, nil
	}
	rec.SID = st.sid
	if rec.AbsoluteExpiry.IsZero() {
		rec.AbsoluteExpiry = now.Add(m.cfg.AbsoluteExpiry)
	}
	return m.cfg.Store.Save(ctx, rec)
}

// commit finalises a request's session state. It is invoked once by
// the middleware just before the response body is written. Returns
// nil on no-op (clean read, or new session that was never touched).
//
// Cookie emission rule: a Set-Cookie is written on every request
// that loaded or established a session and did not destroy it. The
// cookie's expiry tracks min(AbsoluteExpiry, IdleExpiry) so the
// browser drops the cookie at approximately the moment the server
// would consider the session expired anyway — refreshing on every
// commit covers the sliding-expiry case where idle keeps moving
// forward and the original (mint-time) MaxAge would otherwise tick
// down to nothing on the client.
func (m *Manager[T]) commit(ctx context.Context, st *state[T], w http.ResponseWriter) error {
	if st.destroy {
		// Clear the client-side cookie first, unconditionally: the
		// user clicked "log out", so the cookie must not survive
		// even if the store delete fails. A failed delete leaves
		// an orphaned record that will expire on its own; a
		// surviving cookie keeps the user logged in until they
		// notice. Trade transient server-side cleanup against
		// honouring the user's intent.
		m.cfg.Token.Clear(w)
		if st.sid != "" {
			if err := m.cfg.Store.Delete(ctx, st.sid); err != nil {
				return err
			}
		}
		return nil
	}

	if st.renew && st.sid != "" {
		oldSID := st.sid
		// Force commitPayload to mint a fresh SID/Version pair,
		// but preserve UserID, expiry, and any other envelope
		// metadata the caller set (most importantly Promote's
		// UserID assignment).
		st.sid = ""
		st.record.SID = ""
		st.record.Version = 0
		stored, err := m.commitPayload(ctx, st, st.payload)
		if err != nil {
			return err
		}
		st.record = stored
		// Best-effort delete; a failure here leaves a stale record
		// that will expire on its own. The new cookie still goes out.
		_ = m.cfg.Store.Delete(ctx, oldSID)
	} else if st.dirty {
		stored, err := m.commitPayload(ctx, st, st.payload)
		if err != nil {
			return err
		}
		st.record = stored
	} else if st.loaded && st.sid != "" {
		// Read-only request on an existing session. Bump idle
		// expiry if the configured debounce interval has elapsed
		// since the last bump. Falls through quietly when the
		// store can't TTL-bump or the interval is disabled.
		m.maybeBumpIdle(ctx, st)
	}

	if !st.destroy && st.loaded && st.sid != "" {
		m.cfg.Token.Write(w, st.sid, TokenWriteOptions{Expiry: cookieExpiry(st.record)})
	}
	return nil
}

// cookieExpiry returns the moment at which the cookie should be
// dropped by the client — the earlier of AbsoluteExpiry and
// IdleExpiry. Zero values are treated as "no constraint" and skipped.
func cookieExpiry[T any](rec Record[T]) time.Time {
	a, i := rec.AbsoluteExpiry, rec.IdleExpiry
	switch {
	case a.IsZero():
		return i
	case i.IsZero():
		return a
	case i.Before(a):
		return i
	default:
		return a
	}
}

// maybeBumpIdle extends a clean-read session's idle expiry, debounced
// by Config.IdleBumpInterval. Skips silently when the store lacks
// [TTLBumper] (the record will still be re-saved on the next mutating
// request), when bumping is disabled (interval = 0), or when the last
// bump was recent enough that a fresh one is wasted work.
//
// A failure here is logged-and-swallowed conceptually: the request
// already completed successfully and a missed bump just means the
// session reaches idle expiry sooner than it ideally would.
func (m *Manager[T]) maybeBumpIdle(ctx context.Context, st *state[T]) {
	if m.cfg.IdleBumpInterval <= 0 {
		return
	}
	bumper, ok := m.cfg.Store.(TTLBumper)
	if !ok {
		return
	}
	now := m.now()
	// Last bump landed IdleExpiry at (lastBump + IdleExpiry), so
	// remaining = IdleExpiry - now and timeSinceLastBump =
	// IdleExpiry duration - remaining. Bump when timeSinceLastBump
	// >= IdleBumpInterval, i.e. remaining <= IdleExpiry duration
	// - IdleBumpInterval.
	remaining := st.record.IdleExpiry.Sub(now)
	if remaining > m.cfg.IdleExpiry-m.cfg.IdleBumpInterval {
		return
	}
	newExpiry := now.Add(m.cfg.IdleExpiry)
	if err := bumper.BumpTTL(ctx, st.sid, newExpiry); err == nil {
		st.record.IdleExpiry = newExpiry
	}
}
