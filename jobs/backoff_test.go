package jobs

import (
	"testing"
	"time"
)

func TestExponentialBackoff_NoJitter_Doubles(t *testing.T) {
	b := ExponentialBackoff{Base: 1 * time.Second, Max: 1 * time.Hour, Jitter: 0}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},   // attempt < 1 is clamped to 1
		{1, 1 * time.Second},   // Base
		{2, 2 * time.Second},   // Base * 2
		{3, 4 * time.Second},   // Base * 4
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{10, 512 * time.Second},
	}
	for _, tc := range cases {
		got := b.Next(tc.attempt)
		if got != tc.want {
			t.Errorf("attempt=%d: got %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestExponentialBackoff_ClampsToMax(t *testing.T) {
	b := ExponentialBackoff{Base: 1 * time.Second, Max: 1 * time.Hour, Jitter: 0}
	// 2^12 = 4096s = 1h08m, above Max.
	if got := b.Next(13); got != b.Max {
		t.Errorf("attempt=13: got %v, want Max=%v", got, b.Max)
	}
	// Pathological attempt counts must not overflow.
	if got := b.Next(1000); got != b.Max {
		t.Errorf("attempt=1000: got %v, want Max=%v", got, b.Max)
	}
	if got := b.Next(64); got != b.Max {
		t.Errorf("attempt=64 (shift edge): got %v, want Max=%v", got, b.Max)
	}
}

func TestExponentialBackoff_JitterWithinBounds(t *testing.T) {
	b := ExponentialBackoff{Base: 1 * time.Second, Max: 1 * time.Hour, Jitter: 0.2}
	// Attempt 4 deterministic value: 8s. With 20% jitter the
	// returned duration must land in [6.4s, 9.6s] inclusive.
	const attempt = 4
	const center = 8 * time.Second
	low := center - time.Duration(0.2*float64(center))
	high := center + time.Duration(0.2*float64(center))
	for i := 0; i < 1000; i++ {
		got := b.Next(attempt)
		if got < low || got > high {
			t.Fatalf("attempt=%d iter=%d: %v outside [%v, %v]", attempt, i, got, low, high)
		}
	}
}

func TestExponentialBackoff_NeverNegative(t *testing.T) {
	// A pathological Jitter of 1.0 means span == d, so the lower
	// bound is exactly 0. Run a few thousand iterations and verify
	// no negative durations escape.
	b := ExponentialBackoff{Base: 1 * time.Millisecond, Max: 1 * time.Hour, Jitter: 1.0}
	for i := 0; i < 5000; i++ {
		if got := b.Next(1); got < 0 {
			t.Fatalf("got negative duration %v on iter %d", got, i)
		}
	}
}

func TestEncodeDecodeBackoff_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Backoff
		want Backoff
	}{
		{
			name: "value receiver",
			in:   ExponentialBackoff{Base: 5 * time.Second, Max: 1 * time.Hour, Jitter: 0.1},
			want: ExponentialBackoff{Base: 5 * time.Second, Max: 1 * time.Hour, Jitter: 0.1},
		},
		{
			name: "pointer receiver",
			in:   &ExponentialBackoff{Base: 100 * time.Millisecond, Max: 10 * time.Second, Jitter: 0},
			want: ExponentialBackoff{Base: 100 * time.Millisecond, Max: 10 * time.Second, Jitter: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := encodeBackoff(tc.in)
			if len(data) == 0 {
				t.Fatal("encode returned empty bytes")
			}
			got := decodeBackoff(data)
			if got != tc.want {
				t.Errorf("round trip = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestEncodeBackoff_UnsupportedTypeReturnsNil(t *testing.T) {
	if data := encodeBackoff(stubBackoff{}); data != nil {
		t.Errorf("non-ExponentialBackoff should not serialise; got %s", data)
	}
	if data := encodeBackoff(nil); data != nil {
		t.Errorf("nil should not serialise; got %s", data)
	}
}

type stubBackoff struct{}

func (stubBackoff) Next(_ int) time.Duration { return time.Second }

func TestDefaultBackoff_Shape(t *testing.T) {
	// Pin the published defaults so accidental edits to the var
	// surface as a failing test rather than a behaviour change.
	d, ok := DefaultBackoff.(ExponentialBackoff)
	if !ok {
		t.Fatalf("DefaultBackoff is %T, want ExponentialBackoff", DefaultBackoff)
	}
	if d.Base != 1*time.Second {
		t.Errorf("Base = %v, want 1s", d.Base)
	}
	if d.Max != 1*time.Hour {
		t.Errorf("Max = %v, want 1h", d.Max)
	}
	if d.Jitter != 0.2 {
		t.Errorf("Jitter = %v, want 0.2", d.Jitter)
	}
}
