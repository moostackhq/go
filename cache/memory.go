package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Memory is an in-process [Store]: a map guarded by a mutex, with lazy
// TTL expiry, optional LRU eviction by entry count, and an optional
// background janitor for active expiry. Construct with [NewMemory]; the
// zero value is not usable. Safe for concurrent use.
type Memory struct {
	mu         sync.Mutex
	items      map[string]*list.Element // key → element holding *memItem
	lru        *list.List               // front = most recently used
	maxEntries int
	now        func() time.Time

	janitorStop chan struct{}
	janitorOnce sync.Once
}

type memItem struct {
	key     string
	value   []byte
	expires time.Time // zero means no expiry
}

// MemoryOption configures a [Memory] store.
type MemoryOption func(*Memory)

// WithMaxEntries caps the number of entries; once exceeded, the
// least-recently-used entry is evicted. Zero (the default) is unbounded.
func WithMaxEntries(n int) MemoryOption {
	return func(m *Memory) { m.maxEntries = n }
}

// WithJanitor runs a background goroutine that sweeps expired entries
// every interval. Without it, expired entries are reclaimed lazily on
// access (and on eviction). Call [Memory.Close] to stop the janitor.
func WithJanitor(every time.Duration) MemoryOption {
	return func(m *Memory) {
		if every > 0 {
			m.startJanitor(every)
		}
	}
}

// NewMemory returns an in-process store.
func NewMemory(opts ...MemoryOption) *Memory {
	m := &Memory{
		items: make(map[string]*list.Element),
		lru:   list.New(),
		now:   time.Now,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Get implements [Store]. It returns a copy of the stored bytes.
func (m *Memory) Get(ctx context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	it := el.Value.(*memItem)
	if m.expired(it) {
		m.removeElement(el)
		return nil, false, nil
	}
	m.lru.MoveToFront(el)
	out := make([]byte, len(it.value))
	copy(out, it.value)
	return out, true, nil
}

// Set implements [Store]. It stores a copy of value.
func (m *Memory) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	stored := make([]byte, len(value))
	copy(stored, value)
	var expires time.Time
	if ttl > 0 {
		expires = m.now().Add(ttl)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		it := el.Value.(*memItem)
		it.value = stored
		it.expires = expires
		m.lru.MoveToFront(el)
		return nil
	}
	el := m.lru.PushFront(&memItem{key: key, value: stored, expires: expires})
	m.items[key] = el
	if m.maxEntries > 0 && m.lru.Len() > m.maxEntries {
		if oldest := m.lru.Back(); oldest != nil {
			m.removeElement(oldest)
		}
	}
	return nil
}

// Delete implements [Store].
func (m *Memory) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.removeElement(el)
	}
	return nil
}

// Close stops the background janitor if one was started. It is safe to
// call more than once; a store without a janitor needs no Close.
func (m *Memory) Close() error {
	m.janitorOnce.Do(func() {
		if m.janitorStop != nil {
			close(m.janitorStop)
		}
	})
	return nil
}

// expired reports whether it has a TTL that has passed. Caller holds mu.
func (m *Memory) expired(it *memItem) bool {
	return !it.expires.IsZero() && !m.now().Before(it.expires)
}

// removeElement drops el from both the list and the map. Caller holds mu.
func (m *Memory) removeElement(el *list.Element) {
	m.lru.Remove(el)
	delete(m.items, el.Value.(*memItem).key)
}

func (m *Memory) startJanitor(every time.Duration) {
	m.janitorStop = make(chan struct{})
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.reapExpired()
			case <-m.janitorStop:
				return
			}
		}
	}()
}

func (m *Memory) reapExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for el := m.lru.Back(); el != nil; {
		prev := el.Prev()
		if m.expired(el.Value.(*memItem)) {
			m.removeElement(el)
		}
		el = prev
	}
}
