// Package session provides a typed, capability-driven HTTP session manager.
//
// A [Manager] is parameterised by a single payload type T. Handlers
// reach the payload through [Manager.Get] (lazy load, returns *T),
// [Manager.Update] (atomic read-modify-write with CAS retry), or by
// mutating the pointer from Get and calling [Manager.Save] before the
// response is written. Identity-aware operations — [Manager.Promote],
// [Manager.ListForUser], [Manager.RevokeAllForUser] — round out the
// surface for login flows and "log out other devices."
//
// The package ships an in-memory store ([MemoryStore]), a SQLite store
// ([SQLiteStore]), a JSON codec ([JSONCodec]), and cookie and bearer
// token transports ([Cookie], [Bearer], [Multi]). Stores declare their
// capabilities via optional interfaces ([TTLBumper], [UserIndexer],
// [Scanner], [Sweeper]); the manager dispatches via type assertion
// and surfaces [ErrCapabilityMissing] when a caller requests a
// feature the configured store does not implement.
//
// Concurrency is optimistic by default: every [Record] carries a
// Version that the store CAS-checks on save. On conflict,
// [Manager.Update] retries the supplied closure up to Config.MaxRetries
// times. Callers that need to coordinate session writes with external
// side effects (sending email, charging cards) should not use Update,
// since the closure may run more than once.
package session

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var (
	// ErrNotFound is wrapped when a session ID does not resolve to a
	// live record (either it never existed, was deleted, or has passed
	// its absolute or idle expiry).
	ErrNotFound = errors.New("session not found")

	// ErrVersionConflict is wrapped by stores that support CAS when the
	// record's Version does not match the loaded value. [Manager.Update]
	// catches this and retries the closure.
	ErrVersionConflict = errors.New("session version conflict")

	// ErrCapabilityMissing is wrapped when a [Manager] method requires
	// a capability the configured store does not implement (for
	// example, calling RevokeAllForUser against a store that does not
	// implement [UserIndexer]). It is detected via type assertion at
	// the call site.
	ErrCapabilityMissing = errors.New("store capability missing")

	// ErrNoSession is returned by [Manager.Get] and friends when no
	// session state is attached to the context — almost always because
	// the request did not pass through [Manager.Middleware].
	ErrNoSession = errors.New("no session attached to context")

	// ErrSessionDestroyed is wrapped by mutating [Manager] methods
	// (Save, Update, Renew, Promote) when [Manager.Destroy] has
	// already been called for the same request. Destruction is
	// terminal within a request: once the handler asks for the
	// session to go away, subsequent writes are a programming
	// mistake, not a recoverable state.
	ErrSessionDestroyed = errors.New("session is marked for destruction")
)

// Record is the unit a [Store] persists. The payload T is whatever
// domain type the [Manager] was instantiated with; everything else is
// session metadata the store and manager need.
//
// Version is the CAS token. Stores must reject Save when the on-disk
// version does not match Record.Version and must increment it on
// successful write.
//
// AbsoluteExpiry is a hard deadline that never extends. IdleExpiry
// slides forward as the session is used; stores that implement
// [TTLBumper] may update it without rewriting Payload.
type Record[T any] struct {
	SID            string
	Version        uint64
	UserID         string
	AbsoluteExpiry time.Time
	IdleExpiry     time.Time
	Payload        T
}

// Store is the persistence contract for session records. Concrete
// implementations live in store_<backend>.go files. Optional
// behaviours (TTL bumping, user-id indexing, scanning, bulk
// sweeping) are advertised by also implementing the matching
// optional interfaces ([TTLBumper], [UserIndexer], [Scanner],
// [Sweeper]); the manager dispatches via type assertion and
// surfaces [ErrCapabilityMissing] when a feature is requested
// against a store that does not implement it.
//
// Save must honour Record.Version for CAS: if the on-disk record's
// version does not match, Save returns an error wrapping
// [ErrVersionConflict] and does not mutate the stored record. On
// success, Save increments the version and returns the stored Record
// so the caller does not have to mirror the bookkeeping. The
// in-memory Record passed in is not mutated.
//
// Load returns ErrNotFound for records that have passed their
// expiry; pruning is the store's responsibility and is best-effort.
type Store[T any] interface {
	Load(ctx context.Context, sid string) (Record[T], error)
	Save(ctx context.Context, rec Record[T]) (Record[T], error)
	Delete(ctx context.Context, sid string) error
}

// TTLBumper is implemented by stores that can extend a record's idle
// expiry without rewriting its payload. The manager uses this to keep
// sliding-expiry writes off the CAS path, since concurrent payload
// writers and concurrent TTL bumpers should not conflict.
type TTLBumper interface {
	BumpTTL(ctx context.Context, sid string, until time.Time) error
}

// UserIndexer is implemented by stores that maintain a secondary
// index from user ID to session IDs. Required for
// [Manager.ListForUser] and [Manager.RevokeAllForUser]; calling
// either against a store that does not implement UserIndexer returns
// an error wrapping [ErrCapabilityMissing].
//
// ListByUser returns only live sessions — expired-but-unswept rows
// are filtered out so the result matches what Load would let
// callers retrieve. RevokeByUser deletes every matching row
// (including expired ones), since the operation's intent is "remove
// this user's sessions" and a not-yet-swept expired row is still a
// row on disk; the returned count reflects total deletions.
type UserIndexer interface {
	ListByUser(ctx context.Context, userID string) ([]string, error)
	RevokeByUser(ctx context.Context, userID string, except ...string) (int, error)
}

// Scanner is implemented by stores that can iterate every live session.
// Intended for janitorial sweeps and debugging, not request-path work.
type Scanner interface {
	Scan(ctx context.Context, fn func(sid string) bool) error
}

// Sweeper is implemented by stores that can bulk-delete expired
// records. Both [MemoryStore] and [SQLiteStore] satisfy it; the
// interface exists so scheduler / janitor code can be written
// against a single contract instead of type-asserting each store.
//
// Calling Sweep is never required for correctness — reads filter
// expired records — but it bounds on-disk or in-memory growth.
// The returned count is the number of records removed.
type Sweeper interface {
	Sweep(ctx context.Context) (int, error)
}

// Codec encodes and decodes a session payload to and from bytes.
// Stores choose how to lay out envelope metadata (sid, version,
// expiry, user id): a SQL store typically puts each in its own column,
// while an opaque-byte store would wrap the codec output in a framing
// envelope. Either way, the codec itself only deals with the payload
// T, not metadata.
//
// Schema evolution is handled at the codec level: JSON tolerates
// added and removed fields, and a [SQLiteStore] with the default
// graceful decode treats a hard decode failure as if the row never
// existed (the user is silently re-issued a fresh session). Apps
// that need explicit migrations between payload versions should
// version the type themselves (embed a Version field, branch in
// Decode) rather than rely on the codec to do it.
type Codec[T any] interface {
	Encode(T) ([]byte, error)
	Decode([]byte) (T, error)
}

// TokenWriteOptions describes how a token transport should emit the
// session ID to the response. Expiry is the moment at which the
// client should discard the token; transports that lack an
// expiration mechanism (e.g. bearer headers) ignore it. The
// [Manager] computes this as min(AbsoluteExpiry, IdleExpiry) so the
// client drops the token at the earliest moment the server would
// already consider the session over.
type TokenWriteOptions struct {
	Expiry time.Time
}

// Token is the request-side and response-side transport for a session
// ID. The package ships [Cookie] (browser), [Bearer] (header), and
// [Multi] (compose two or more, e.g. cookie + bearer for SPA+SSR
// hybrids).
type Token interface {
	Read(*http.Request) (sid string, ok bool)
	Write(w http.ResponseWriter, sid string, opts TokenWriteOptions)
	Clear(w http.ResponseWriter)
}
