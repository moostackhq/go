// Package cache is a small, loss-safe cache: a typed front ([Cache])
// over a byte-oriented backend ([Store]), with TTL, optional eviction,
// and single-flight loading to absorb stampedes.
//
// The contract is the point. A cached value must always be
// reconstructible from its source, because the cache may drop it at any
// moment — TTL expiry, eviction under pressure, or a process restart.
// Never store anything here that isn't recomputable; durable state
// belongs in a database.
//
//	c := cache.New[Rollup](cache.NewMemory(), cache.WithNamespace("rollups"))
//	r, err := c.GetOrLoad(ctx, key, 30*time.Second, func(ctx context.Context) (Rollup, error) {
//	    return svc.computeRollup(ctx) // runs at most once across concurrent callers
//	})
//
// The byte-oriented Store is one backend serving many typed caches; the
// generic Cache[T] marshals values through a [Codec] (JSON by default).
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Store is the cache backend: byte values keyed by string, each with an
// optional TTL. A ttl <= 0 means no expiry — the entry lives until it is
// evicted or deleted. A miss is (nil, false, nil); only a backend
// failure returns a non-nil error. Implementations must not hand the
// caller a slice that aliases their internal storage (return a copy), so
// the caller cannot corrupt cached data by mutating it.
type Store interface {
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// Codec marshals values to and from the bytes a [Store] holds. The
// default is JSON; override with [WithCodec].
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// Cache is a typed view over a [Store]: it marshals T through a [Codec]
// and adds single-flight loading. Construct with [New]. A Cache is safe
// for concurrent use.
type Cache[T any] struct {
	store     Store
	codec     Codec
	namespace string
	onError   func(op, key string, err error)
	flight    *flightGroup
}

type config struct {
	codec     Codec
	namespace string
	onError   func(op, key string, err error)
}

// Option configures a [Cache].
type Option func(*config)

// WithNamespace prefixes every key with namespace + ":", so several
// caches sharing one [Store] do not collide. The separator is not
// escaped: namespace "a" with key "b:c" and namespace "a:b" with key "c"
// both resolve to "a:b:c". Use distinct, fixed namespace literals (the
// intended usage) and that ambiguity never arises.
func WithNamespace(namespace string) Option {
	return func(c *config) { c.namespace = namespace }
}

// WithCodec sets the value codec (default JSON).
func WithCodec(codec Codec) Option {
	return func(c *config) { c.codec = codec }
}

// WithErrorHook registers a callback for the cache faults that
// [Cache.GetOrLoad] otherwise swallows: a failed read ("get") and a
// failed write after a successful load ("set"). It is the seam for
// noticing a structurally broken cache — e.g. a value type the codec
// can't encode — which would otherwise silently re-load on every call.
// The hook must not block; it runs on the calling goroutine.
func WithErrorHook(fn func(op, key string, err error)) Option {
	return func(c *config) { c.onError = fn }
}

// New returns a Cache[T] backed by store.
func New[T any](store Store, opts ...Option) *Cache[T] {
	cfg := config{codec: jsonCodec{}}
	for _, o := range opts {
		o(&cfg)
	}
	return &Cache[T]{
		store:     store,
		codec:     cfg.codec,
		namespace: cfg.namespace,
		onError:   cfg.onError,
		flight:    newFlightGroup(),
	}
}

func (c *Cache[T]) reportError(op, key string, err error) {
	if c.onError != nil {
		c.onError(op, key, err)
	}
}

func (c *Cache[T]) key(k string) string {
	if c.namespace == "" {
		return k
	}
	return c.namespace + ":" + k
}

// Get returns the value for key. The bool is false on a miss; err is
// non-nil only on a backend or decode failure.
func (c *Cache[T]) Get(ctx context.Context, key string) (T, bool, error) {
	var zero T
	data, ok, err := c.store.Get(ctx, c.key(key))
	if err != nil {
		return zero, false, err
	}
	if !ok {
		return zero, false, nil
	}
	var v T
	if err := c.codec.Unmarshal(data, &v); err != nil {
		return zero, false, fmt.Errorf("cache: decode %q: %w", key, err)
	}
	return v, true, nil
}

// Set stores v under key with the given ttl (<= 0 means no expiry).
func (c *Cache[T]) Set(ctx context.Context, key string, v T, ttl time.Duration) error {
	data, err := c.codec.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache: encode %q: %w", key, err)
	}
	return c.store.Set(ctx, c.key(key), data, ttl)
}

// Delete removes key. Deleting an absent key is not an error.
func (c *Cache[T]) Delete(ctx context.Context, key string) error {
	return c.store.Delete(ctx, c.key(key))
}

// GetOrLoad returns the cached value for key, or calls load to produce
// it, stores it under ttl, and returns it.
//
// Concurrent calls for the same key share a single load (single-flight):
// load runs once and every caller receives its result, so a cold key
// under load does not stampede the source. A load error is returned to
// all waiters and nothing is cached. Single-flight is scoped to this
// Cache instance — two separate caches over the same Store do not share
// a flight for the same key. If load panics, the panic is re-raised in
// every concurrent caller, not turned into a nil result.
//
// The cache is best-effort: a failed cache read is treated as a miss
// (load still runs), and a failed write after a successful load is
// ignored (the loaded value is returned regardless) — losing a cache
// operation must never deny the caller a valid value. Both swallowed
// faults are reported to a [WithErrorHook] callback when one is set.
//
// load (and any error hook) must not re-enter GetOrLoad for the same
// key: the in-flight call is still holding that key, so the nested call
// would block forever waiting on itself.
func (c *Cache[T]) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load func(context.Context) (T, error)) (T, error) {
	if v, ok, err := c.Get(ctx, key); ok {
		return v, nil
	} else if err != nil {
		c.reportError("get", key, err)
	}
	v, err := c.flight.Do(key, func() (any, error) {
		// A concurrent caller may have populated the key between our
		// miss above and entering the flight; re-check for a hit before
		// loading. A read error here is the same fault the outer Get
		// already reported for this call, so don't report it twice —
		// just fall through to load.
		if v, ok, _ := c.Get(ctx, key); ok {
			return v, nil
		}
		loaded, err := load(ctx)
		if err != nil {
			return loaded, err
		}
		if err := c.Set(ctx, key, loaded, ttl); err != nil {
			c.reportError("set", key, err) // best-effort; value is valid regardless
		}
		return loaded, nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return v.(T), nil
}
