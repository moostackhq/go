package jobs

import (
	"encoding/json"
	"math/rand/v2"
	"time"
)

// Backoff computes the delay before a job's next attempt. Implementations
// must be safe for concurrent use; the runtime may call Next from any
// worker goroutine.
type Backoff interface {
	Next(attempt int) time.Duration
}

// ExponentialBackoff doubles the delay each attempt, clamped to Max,
// with symmetric jitter applied last. Jitter of 0.2 means the
// returned duration is within +/-20% of the deterministic value.
type ExponentialBackoff struct {
	Base   time.Duration
	Max    time.Duration
	Jitter float64
}

// DefaultBackoff is what the manager uses when neither Options.Backoff
// nor Config.DefaultBackoff is set: 1s base, 1h cap, 20% jitter.
var DefaultBackoff Backoff = ExponentialBackoff{
	Base:   1 * time.Second,
	Max:    1 * time.Hour,
	Jitter: 0.2,
}

// backoffSpec is the persistence shape of a per-job [Backoff]
// override. The runner serialises Options.Backoff into JSON on
// enqueue and rehydrates it before each retry, so the per-job
// schedule survives crashes and lease handoffs between workers.
//
// Type tags the strategy; today only "exponential" is recognised,
// which means [Options.Backoff] of any other concrete type is not
// persisted and the manager's DefaultBackoff is used on retry.
type backoffSpec struct {
	Type   string        `json:"type"`
	Base   time.Duration `json:"base,omitempty"`
	Max    time.Duration `json:"max,omitempty"`
	Jitter float64       `json:"jitter,omitempty"`
}

// encodeBackoff returns the JSON-encoded spec for b, or nil when b
// is nil or of a type we cannot serialise. Returning nil tells the
// runner to fall back to [Config.DefaultBackoff] on retry.
func encodeBackoff(b Backoff) []byte {
	if b == nil {
		return nil
	}
	var exp ExponentialBackoff
	switch v := b.(type) {
	case ExponentialBackoff:
		exp = v
	case *ExponentialBackoff:
		if v == nil {
			return nil
		}
		exp = *v
	default:
		return nil
	}
	data, err := json.Marshal(backoffSpec{
		Type:   "exponential",
		Base:   exp.Base,
		Max:    exp.Max,
		Jitter: exp.Jitter,
	})
	if err != nil {
		return nil
	}
	return data
}

// decodeBackoff reconstructs a Backoff from its serialised spec.
// Returns nil for empty input or unrecognised types; callers fall
// back to the manager default.
func decodeBackoff(data []byte) Backoff {
	if len(data) == 0 {
		return nil
	}
	var spec backoffSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil
	}
	switch spec.Type {
	case "exponential":
		return ExponentialBackoff{
			Base:   spec.Base,
			Max:    spec.Max,
			Jitter: spec.Jitter,
		}
	}
	return nil
}

// Next returns Base * 2^(attempt-1), clamped to Max, with jitter
// applied. attempt < 1 is treated as 1 so callers never need to
// special-case the first call.
func (b ExponentialBackoff) Next(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Shift by 63 or more would overflow int64; clamp early.
	shift := uint(attempt - 1)
	d := b.Max
	if shift < 63 {
		candidate := b.Base << shift
		// Guard against signed overflow when Base * 2^shift exceeds
		// int64. Negative candidate means we overflowed.
		if candidate > 0 && candidate < b.Max {
			d = candidate
		}
	}
	if b.Jitter > 0 && d > 0 {
		span := time.Duration(float64(d) * b.Jitter)
		if span > 0 {
			// Uniform in [-span, +span]. rand/v2 funcs are
			// goroutine-safe and seeded automatically.
			d = d - span + time.Duration(rand.Int64N(int64(2*span)+1))
			if d < 0 {
				d = 0
			}
		}
	}
	return d
}
