// Package ratelimit is a small, backend-pluggable rate limiter built
// on the GCRA (generic cell rate) algorithm — a token bucket that
// stores a single timestamp per key and yields an exact retry-after.
//
// Three layers, usable independently:
//
//   - [Limiter] — the core mechanism: Allow(ctx, key) reports whether
//     a request fits the limit and by how much. No HTTP, no globals.
//   - [Store] — the pluggable backend (a key→value store with TTL and
//     compare-and-swap). [MemoryStore] ships in-package.
//   - [Middleware] — an http.Handler wrapper for routes or route
//     groups: keys by client IP (or your own [KeyFunc]), emits
//     RateLimit-* / Retry-After headers, and returns 429.
//
// Two things to keep in mind:
//
//   - [MemoryStore] is per-process. Behind multiple replicas each
//     process keeps its own buckets, so the effective limit is N× the
//     configured one. Use a shared backend for a global limit.
//   - Deriving the client key from a request is security-sensitive:
//     X-Forwarded-For is spoofable. [KeyByIP] uses RemoteAddr only;
//     opt into proxy headers explicitly.
package ratelimit

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrInvalidLimit is returned by [New] for a malformed Limit: a
// non-positive Rate, Period, or Burst, a Rate so high the per-request
// interval rounds to zero, or a Burst so large the burst window
// overflows.
var ErrInvalidLimit = errors.New("ratelimit: invalid limit")

// Limit describes an allowed rate: Rate requests per Period, with
// bursting up to Burst requests held in reserve.
//
//	ratelimit.PerMinute(100)              // 100/min, burst 100
//	ratelimit.PerMinute(100).WithBurst(20) // steady 100/min, absorb 20 at once
//
// Burst is the bucket capacity. When zero it defaults to Rate (a full
// period's worth can be spent at once). Set Burst to 1 to forbid
// bursting entirely (requests must be spaced Period/Rate apart).
type Limit struct {
	Rate   int
	Period time.Duration
	Burst  int
}

// PerSecond returns a Limit of n requests per second.
func PerSecond(n int) Limit { return Limit{Rate: n, Period: time.Second} }

// PerMinute returns a Limit of n requests per minute.
func PerMinute(n int) Limit { return Limit{Rate: n, Period: time.Minute} }

// PerHour returns a Limit of n requests per hour.
func PerHour(n int) Limit { return Limit{Rate: n, Period: time.Hour} }

// WithBurst returns a copy of l with Burst set.
func (l Limit) WithBurst(burst int) Limit {
	l.Burst = burst
	return l
}

// capacity is the bucket size: Burst when set, else Rate.
func (l Limit) capacity() int {
	if l.Burst > 0 {
		return l.Burst
	}
	return l.Rate
}

// emission is the steady-state interval between requests.
func (l Limit) emission() time.Duration {
	return l.Period / time.Duration(l.Rate)
}

func (l Limit) validate() error {
	if l.Rate <= 0 {
		return fmt.Errorf("%w: Rate must be > 0", ErrInvalidLimit)
	}
	if l.Period <= 0 {
		return fmt.Errorf("%w: Period must be > 0", ErrInvalidLimit)
	}
	if l.Burst < 0 {
		return fmt.Errorf("%w: Burst must be >= 0", ErrInvalidLimit)
	}
	// Guard the derived values the limiter divides and multiplies by.
	// emission truncates to 0 when Rate exceeds Period in nanoseconds,
	// which would divide-by-zero in the limiter; and emission*capacity
	// (the burst tolerance) must fit in a time.Duration.
	emission := l.emission()
	if emission <= 0 {
		return fmt.Errorf("%w: Rate %d too high for Period %s (interval rounds to zero)", ErrInvalidLimit, l.Rate, l.Period)
	}
	if emission > time.Duration(math.MaxInt64)/time.Duration(l.capacity()) {
		return fmt.Errorf("%w: Burst %d too large for Period %s (burst window overflows)", ErrInvalidLimit, l.capacity(), l.Period)
	}
	return nil
}

// Result is the outcome of one [Limiter.Allow] / [Limiter.AllowN].
type Result struct {
	// Allowed reports whether the request fits within the limit.
	Allowed bool
	// Limit is the bucket capacity (for the RateLimit-Limit header).
	Limit int
	// Remaining is the number of further requests allowed right now.
	Remaining int
	// RetryAfter is how long until a denied request would be allowed;
	// zero when Allowed.
	RetryAfter time.Duration
	// ResetAfter is how long until the bucket is fully replenished.
	ResetAfter time.Duration
}
