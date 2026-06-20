package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process [Store] backed by a map. Entries expire
// lazily on access; for keyspaces that churn (e.g. per-IP keys that
// never recur) start a background sweeper with [WithCleanupInterval]
// to reclaim them, and call [MemoryStore.Close] to stop it.
//
// It is per-process: not shared across replicas. See the package doc.
type MemoryStore struct {
	mu        sync.Mutex
	m         map[string]memEntry
	now       func() time.Time
	stop      chan struct{}
	closeOnce sync.Once
}

type memEntry struct {
	value     int64
	expiresAt time.Time // zero = never
}

// MemoryOption configures a [MemoryStore].
type MemoryOption func(*MemoryStore)

// WithCleanupInterval starts a background goroutine that evicts expired
// entries every interval. Without it, entries are only reclaimed when
// their key is next accessed. Call [MemoryStore.Close] to stop it.
func WithCleanupInterval(interval time.Duration) MemoryOption {
	return func(s *MemoryStore) {
		if interval <= 0 {
			return
		}
		s.stop = make(chan struct{})
		go s.janitor(interval, s.stop)
	}
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore(opts ...MemoryOption) *MemoryStore {
	s := &MemoryStore{m: make(map[string]memEntry), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Get implements [Store].
func (s *MemoryStore) Get(_ context.Context, key string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(key)
	return e.value, ok, nil
}

// SetIfAbsent implements [Store].
func (s *MemoryStore) SetIfAbsent(_ context.Context, key string, value int64, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live(key); ok {
		return false, nil
	}
	s.m[key] = memEntry{value: value, expiresAt: s.expiry(ttl)}
	return true, nil
}

// CompareAndSwap implements [Store].
func (s *MemoryStore) CompareAndSwap(_ context.Context, key string, oldValue, newValue int64, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(key)
	if !ok || e.value != oldValue {
		return false, nil
	}
	s.m[key] = memEntry{value: newValue, expiresAt: s.expiry(ttl)}
	return true, nil
}

// Close stops the background sweeper, if one was started. Idempotent;
// a no-op on a store with no sweeper.
func (s *MemoryStore) Close() error {
	if s.stop != nil {
		s.closeOnce.Do(func() { close(s.stop) })
	}
	return nil
}

// live returns the entry for key, deleting and reporting absent if it
// has expired. Caller holds the lock.
func (s *MemoryStore) live(key string) (memEntry, bool) {
	e, ok := s.m[key]
	if !ok {
		return memEntry{}, false
	}
	if !e.expiresAt.IsZero() && !s.now().Before(e.expiresAt) {
		delete(s.m, key)
		return memEntry{}, false
	}
	return e, true
}

func (s *MemoryStore) expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return s.now().Add(ttl)
}

func (s *MemoryStore) janitor(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *MemoryStore) sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if !e.expiresAt.IsZero() && !now.Before(e.expiresAt) {
			delete(s.m, k)
		}
	}
}
