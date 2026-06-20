package ratelimit

import (
	"context"
	"errors"
	"time"
)

// ErrContended is a termination backstop, returned only if the
// compare-and-swap loop fails to commit within a budget far larger
// than realistic contention needs (see [New]). It is effectively
// unreachable in normal operation: a lost CAS means another request
// was admitted, so contention is self-limiting. Callers (e.g.
// [Middleware]) decide whether to fail open or closed if it surfaces.
var ErrContended = errors.New("ratelimit: store contended")

// minRetryBudget floors the CAS-retry backstop so small-capacity
// limiters still tolerate bursts of contention without erroring.
const minRetryBudget = 1024

// Limiter enforces one [Limit] against a [Store]. It is safe for
// concurrent use. Construct with [New].
type Limiter struct {
	store     Store
	capacity  int
	emission  time.Duration
	dvt       time.Duration // emission * capacity — the burst tolerance
	namespace string
	now       func() time.Time
	maxRetry  int
}

// Option configures a [Limiter].
type Option func(*Limiter)

// WithNamespace prefixes every store key, so multiple limiters can
// share one [Store] without colliding on the same caller key (e.g.
// two route groups both keyed by IP). Default: empty.
func WithNamespace(ns string) Option {
	return func(l *Limiter) { l.namespace = ns }
}

// WithClock overrides the time source. Intended for tests.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) { l.now = now }
}

// New returns a Limiter enforcing limit against store. It returns
// [ErrInvalidLimit] if the limit is malformed.
func New(store Store, limit Limit, opts ...Option) (*Limiter, error) {
	if err := limit.validate(); err != nil {
		return nil, err
	}
	l := &Limiter{
		store:    store,
		capacity: limit.capacity(),
		emission: limit.emission(),
		now:      time.Now,
	}
	l.dvt = l.emission * time.Duration(l.capacity)
	// Each lost CAS means another request was admitted, so for a
	// saturated bucket a would-be-admitted request loses ~capacity
	// races before it cleanly sees "denied"; a live, refilling clock
	// can add some churn on top. Budget well beyond that — a backstop
	// against a broken store, not a normal-path limit.
	l.maxRetry = l.capacity * 2
	if l.maxRetry < minRetryBudget {
		l.maxRetry = minRetryBudget
	}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// Allow reports whether one request for key fits within the limit,
// consuming a slot when it does.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	return l.AllowN(ctx, key, 1)
}

// AllowN is Allow for n requests at once. An n larger than the bucket
// capacity can never be allowed (denied with RetryAfter 0). n <= 0 is
// a no-op that always allows and reports full Remaining without
// consulting the store.
func (l *Limiter) AllowN(ctx context.Context, key string, n int) (Result, error) {
	if n <= 0 {
		return Result{Allowed: true, Limit: l.capacity, Remaining: l.capacity}, nil
	}
	if n > l.capacity {
		// Larger than the bucket can ever hold — denied regardless of
		// state, and retrying never helps, so RetryAfter is zero.
		return Result{Allowed: false, Limit: l.capacity}, nil
	}
	storeKey := l.key(key)
	increment := l.emission * time.Duration(n)

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		stored, exists, err := l.store.Get(ctx, storeKey)
		if err != nil {
			return Result{}, err
		}

		now := l.now()
		tat := now
		if exists {
			if t := time.Unix(0, stored); t.After(now) {
				tat = t
			}
		}

		newTat := tat.Add(increment)
		allowAt := newTat.Add(-l.dvt)
		if now.Before(allowAt) {
			return Result{
				Allowed:    false,
				Limit:      l.capacity,
				Remaining:  0,
				RetryAfter: allowAt.Sub(now),
				ResetAfter: tat.Sub(now),
			}, nil
		}

		ttl := newTat.Sub(now)
		var ok bool
		if exists {
			ok, err = l.store.CompareAndSwap(ctx, storeKey, stored, newTat.UnixNano(), ttl)
		} else {
			ok, err = l.store.SetIfAbsent(ctx, storeKey, newTat.UnixNano(), ttl)
		}
		if err != nil {
			return Result{}, err
		}
		if ok {
			remaining := int((l.dvt - newTat.Sub(now)) / l.emission)
			if remaining < 0 {
				remaining = 0
			}
			return Result{
				Allowed:    true,
				Limit:      l.capacity,
				Remaining:  remaining,
				ResetAfter: newTat.Sub(now),
			}, nil
		}
		// Lost the race; another request updated the key. Retry.
		if attempt >= l.maxRetry {
			return Result{}, ErrContended
		}
	}
}

func (l *Limiter) key(k string) string {
	if l.namespace == "" {
		return k
	}
	return l.namespace + ":" + k
}
