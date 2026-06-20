package ratelimit

import (
	"context"
	"time"
)

// Store is the persistence seam for a [Limiter]. It is a key→int64
// store with per-key TTL and compare-and-swap — the limiter holds the
// GCRA algorithm and drives the store with a CAS-retry loop, so a
// backend only has to provide three small atomic operations. The
// value is opaque to the store (it's the limiter's theoretical
// arrival time, in Unix nanoseconds).
//
// Implementations must be safe for concurrent use. Each method is a
// single atomic step; the limiter never assumes state is unchanged
// between calls.
type Store interface {
	// Get returns the value for key and whether it currently exists.
	// An expired entry reports exists=false.
	Get(ctx context.Context, key string) (value int64, exists bool, err error)

	// SetIfAbsent stores value with the given TTL only if key does not
	// currently exist, reporting whether it did so.
	SetIfAbsent(ctx context.Context, key string, value int64, ttl time.Duration) (ok bool, err error)

	// CompareAndSwap stores newValue with the given TTL only if key
	// currently holds oldValue, reporting whether it did so. A missing
	// key compares unequal (ok=false).
	CompareAndSwap(ctx context.Context, key string, oldValue, newValue int64, ttl time.Duration) (ok bool, err error)
}
