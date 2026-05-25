package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryStore is a process-local [Store] backed by a map. It
// implements all four optional capability interfaces ([TTLBumper],
// [UserIndexer], [Scanner], [Sweeper]), which makes it both a
// useful reference for what each one is supposed to mean and a
// zero-config target for tests. It is not suitable for
// multi-process deployments and it never spawns a janitor; expired
// records are pruned lazily on access by [MemoryStore.Load] and in
// bulk by [MemoryStore.Sweep].
//
// All operations are safe for concurrent use. The store holds typed
// records and does not invoke any [Codec]; the codec contract is
// exercised by byte-backed stores such as [SQLiteStore].
type MemoryStore[T any] struct {
	mu        sync.RWMutex
	records   map[string]Record[T]
	userIndex map[string]map[string]struct{}

	now func() time.Time // injectable for tests
}

// NewMemoryStore returns a ready-to-use in-memory store.
func NewMemoryStore[T any]() *MemoryStore[T] {
	return &MemoryStore[T]{
		records:   make(map[string]Record[T]),
		userIndex: make(map[string]map[string]struct{}),
		now:       time.Now,
	}
}

func (s *MemoryStore[T]) Load(ctx context.Context, sid string) (Record[T], error) {
	if err := ctx.Err(); err != nil {
		return Record[T]{}, err
	}
	s.mu.RLock()
	rec, ok := s.records[sid]
	s.mu.RUnlock()
	if !ok {
		return Record[T]{}, ErrNotFound
	}
	if s.expired(rec) {
		// Best-effort prune. Take the write lock and re-check, since
		// the record may have been updated between the RUnlock above
		// and the Lock below.
		s.mu.Lock()
		if cur, ok := s.records[sid]; ok && s.expired(cur) {
			s.deleteLocked(sid)
		}
		s.mu.Unlock()
		return Record[T]{}, ErrNotFound
	}
	return rec, nil
}

func (s *MemoryStore[T]) Save(ctx context.Context, rec Record[T]) (Record[T], error) {
	if err := ctx.Err(); err != nil {
		return Record[T]{}, err
	}
	if rec.SID == "" {
		return Record[T]{}, fmt.Errorf("memorystore: save: empty SID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, exists := s.records[rec.SID]
	if exists {
		if prev.Version != rec.Version {
			return Record[T]{}, fmt.Errorf("memorystore: sid=%s loaded=%d on-disk=%d: %w",
				rec.SID, rec.Version, prev.Version, ErrVersionConflict)
		}
	} else if rec.Version != 0 {
		// Caller claims to have loaded a non-zero version, but
		// nothing is on disk. Treat as a CAS miss rather than
		// silently inserting at version N+1.
		return Record[T]{}, fmt.Errorf("memorystore: sid=%s loaded=%d but no record present: %w",
			rec.SID, rec.Version, ErrVersionConflict)
	}

	stored := rec
	stored.Version++
	s.records[rec.SID] = stored

	// Maintain the user index. Only touch a side when there is
	// actually an entry to add or remove — skipping the empty
	// UserID avoids a redundant map lookup on the common case of
	// a session that has never had an identity attached.
	if exists && prev.UserID != "" && prev.UserID != stored.UserID {
		s.userIndexRemoveLocked(prev.UserID, rec.SID)
	}
	if stored.UserID != "" && (!exists || prev.UserID != stored.UserID) {
		s.userIndexAddLocked(stored.UserID, rec.SID)
	}
	return stored, nil
}

func (s *MemoryStore[T]) Delete(ctx context.Context, sid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(sid)
	return nil
}

// BumpTTL implements [TTLBumper]. It updates IdleExpiry without
// touching Payload and without bumping Version, so concurrent
// payload writers do not see a false CAS conflict from a sliding
// expiry update. Returns [ErrNotFound] for both a missing sid and
// one whose record has already expired — a bump cannot revive a
// session that Load would already refuse to return.
func (s *MemoryStore[T]) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[sid]
	if !ok || s.expired(rec) {
		return ErrNotFound
	}
	rec.IdleExpiry = until
	s.records[sid] = rec
	return nil
}

// ListByUser implements [UserIndexer]. Only live sessions are
// returned — expired-but-unswept rows are filtered out so callers
// observe the same set Load would let them retrieve.
func (s *MemoryStore[T]) ListByUser(ctx context.Context, userID string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.userIndex[userID]
	out := make([]string, 0, len(set))
	for sid := range set {
		if rec, ok := s.records[sid]; ok && !s.expired(rec) {
			out = append(out, sid)
		}
	}
	return out, nil
}

// RevokeByUser implements [UserIndexer]. Any SIDs in except are left
// in place; every other session belonging to userID is deleted.
func (s *MemoryStore[T]) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	keep := make(map[string]struct{}, len(except))
	for _, sid := range except {
		keep[sid] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.userIndex[userID]
	revoked := 0
	for sid := range set {
		if _, kept := keep[sid]; kept {
			continue
		}
		s.deleteLocked(sid)
		revoked++
	}
	return revoked, nil
}

// Scan implements [Scanner]. fn is called once per live SID;
// expired-but-unswept rows are skipped. Iteration stops as soon as
// fn returns false. fn must not call back into the store, which
// would deadlock.
func (s *MemoryStore[T]) Scan(ctx context.Context, fn func(sid string) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	sids := make([]string, 0, len(s.records))
	for sid, rec := range s.records {
		if s.expired(rec) {
			continue
		}
		sids = append(sids, sid)
	}
	s.mu.RUnlock()
	for _, sid := range sids {
		if !fn(sid) {
			return nil
		}
	}
	return nil
}

// Sweep implements [Sweeper]. It deletes every record whose
// absolute or idle expiry has already passed and returns the
// number removed.
//
// Reads already filter expired records, so missing a sweep never
// returns stale data; the only cost is in-memory size.
func (s *MemoryStore[T]) Sweep(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []string
	for sid, rec := range s.records {
		if s.expired(rec) {
			expired = append(expired, sid)
		}
	}
	for _, sid := range expired {
		s.deleteLocked(sid)
	}
	return len(expired), nil
}

func (s *MemoryStore[T]) expired(rec Record[T]) bool {
	now := s.now()
	if !rec.AbsoluteExpiry.IsZero() && !now.Before(rec.AbsoluteExpiry) {
		return true
	}
	if !rec.IdleExpiry.IsZero() && !now.Before(rec.IdleExpiry) {
		return true
	}
	return false
}

func (s *MemoryStore[T]) deleteLocked(sid string) {
	rec, ok := s.records[sid]
	if !ok {
		return
	}
	delete(s.records, sid)
	if rec.UserID != "" {
		s.userIndexRemoveLocked(rec.UserID, sid)
	}
}

func (s *MemoryStore[T]) userIndexAddLocked(userID, sid string) {
	set := s.userIndex[userID]
	if set == nil {
		set = make(map[string]struct{})
		s.userIndex[userID] = set
	}
	set[sid] = struct{}{}
}

func (s *MemoryStore[T]) userIndexRemoveLocked(userID, sid string) {
	set := s.userIndex[userID]
	if set == nil {
		return
	}
	delete(set, sid)
	if len(set) == 0 {
		delete(s.userIndex, userID)
	}
}
