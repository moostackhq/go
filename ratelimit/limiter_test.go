package ratelimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable, concurrency-safe time source shared by
// the tests in this package.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestNew_InvalidLimit(t *testing.T) {
	for _, l := range []Limit{
		{Rate: 0, Period: time.Second},
		{Rate: 5, Period: 0},
		{Rate: 5, Period: time.Second, Burst: -1},
		{Rate: 2_000_000_000, Period: time.Second},     // emission rounds to 0
		{Rate: 1, Period: time.Hour, Burst: 3_000_000}, // emission*capacity overflows
	} {
		if _, err := New(NewMemoryStore(), l); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("New(%+v) err = %v, want ErrInvalidLimit", l, err)
		}
	}

	// The boundary just below sub-nanosecond is valid: 1e9/sec → 1ns
	// emission. It must construct and not panic on use.
	lim, err := New(NewMemoryStore(), PerSecond(1_000_000_000))
	if err != nil {
		t.Fatalf("PerSecond(1e9) should be valid, got %v", err)
	}
	if _, err := lim.Allow(context.Background(), "k"); err != nil {
		t.Fatalf("Allow on boundary limit errored: %v", err)
	}
}

func TestLimiter_BurstThenRefill(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	lim, err := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: 3}, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Full burst of 3, remaining counting down.
	for i, wantRem := range []int{2, 1, 0} {
		res, err := lim.Allow(ctx, "k")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("req %d denied, want allowed", i+1)
		}
		if res.Remaining != wantRem {
			t.Errorf("req %d remaining = %d, want %d", i+1, res.Remaining, wantRem)
		}
		if res.Limit != 3 {
			t.Errorf("req %d Limit = %d, want 3", i+1, res.Limit)
		}
	}

	// Fourth denied; retry-after one emission interval.
	res, err := lim.Allow(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("4th request allowed, want denied")
	}
	if res.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v, want 1s", res.RetryAfter)
	}

	// After one second, exactly one more allowed, then denied again.
	clk.advance(time.Second)
	if res, _ := lim.Allow(ctx, "k"); !res.Allowed {
		t.Fatal("after 1s: denied, want allowed")
	}
	if res, _ := lim.Allow(ctx, "k"); res.Allowed {
		t.Fatal("second request within the same second allowed, want denied")
	}
}

func TestLimiter_AllowN(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: 5}, WithClock(clk.now))
	ctx := context.Background()

	res, err := lim.AllowN(ctx, "k", 5) // exactly capacity
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed || res.Remaining != 0 {
		t.Fatalf("AllowN(5) = %+v, want allowed with remaining 0", res)
	}
	if res, _ := lim.AllowN(ctx, "k", 1); res.Allowed {
		t.Fatal("AllowN(1) on an empty bucket allowed, want denied")
	}
}

func TestLimiter_AllowN_ExceedingCapacityAlwaysDenied(t *testing.T) {
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: 3})
	res, _ := lim.AllowN(context.Background(), "k", 4)
	if res.Allowed {
		t.Fatal("AllowN(4) with capacity 3 allowed, want denied")
	}
	// Retrying never helps for n > capacity, so RetryAfter must be 0.
	if res.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 (n > capacity is hopeless)", res.RetryAfter)
	}
}

func TestLimiter_NamespaceIsolation(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	store := NewMemoryStore()
	a, _ := New(store, Limit{Rate: 1, Period: time.Second, Burst: 1}, WithClock(clk.now), WithNamespace("a"))
	b, _ := New(store, Limit{Rate: 1, Period: time.Second, Burst: 1}, WithClock(clk.now), WithNamespace("b"))
	ctx := context.Background()

	// Same caller key, shared store: each namespace has its own bucket.
	if res, _ := a.Allow(ctx, "k"); !res.Allowed {
		t.Fatal("a: first denied")
	}
	if res, _ := a.Allow(ctx, "k"); res.Allowed {
		t.Fatal("a: second allowed, want denied")
	}
	if res, _ := b.Allow(ctx, "k"); !res.Allowed {
		t.Fatal("b: first denied — namespaces must not collide on the shared store")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: 1}, WithClock(clk.now))
	ctx := context.Background()
	if res, _ := lim.Allow(ctx, "a"); !res.Allowed {
		t.Fatal("a first denied")
	}
	if res, _ := lim.Allow(ctx, "a"); res.Allowed {
		t.Fatal("a second allowed, want denied")
	}
	if res, _ := lim.Allow(ctx, "b"); !res.Allowed {
		t.Fatal("b first denied — keys must be independent")
	}
}

// stuckStore never commits: Get always reports absent and the writes
// always lose. It drives the limiter into its ErrContended backstop.
type stuckStore struct{}

func (stuckStore) Get(context.Context, string) (int64, bool, error) { return 0, false, nil }
func (stuckStore) SetIfAbsent(context.Context, string, int64, time.Duration) (bool, error) {
	return false, nil
}
func (stuckStore) CompareAndSwap(context.Context, string, int64, int64, time.Duration) (bool, error) {
	return false, nil
}

// TestLimiter_ContendedBackstop verifies the CAS loop gives up with
// ErrContended (rather than spinning forever) when the store never
// lets a write land.
func TestLimiter_ContendedBackstop(t *testing.T) {
	lim, _ := New(stuckStore{}, PerSecond(5))
	if _, err := lim.Allow(context.Background(), "k"); !errors.Is(err, ErrContended) {
		t.Fatalf("err = %v, want ErrContended", err)
	}
}

// TestLimiter_ContextCancelled verifies the retry loop honors context
// cancellation — important because MemoryStore's own ops don't.
func TestLimiter_ContextCancelled(t *testing.T) {
	lim, _ := New(stuckStore{}, PerSecond(5)) // would loop without the ctx check
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lim.Allow(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestLimiter_ConcurrentAllow_NoOverAdmission freezes the clock (no
// refill) and hammers one key from many goroutines using the DEFAULT
// retry budget: the CAS loop must admit exactly the bucket capacity —
// never more (over-admission) and never fewer (a spurious
// ErrContended stealing a free slot). Run under -race.
func TestLimiter_ConcurrentAllow_NoOverAdmission(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	const capacity = 50
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: capacity}, WithClock(clk.now))
	ctx := context.Background()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := lim.Allow(ctx, "k"); err == nil && res.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != capacity {
		t.Errorf("allowed %d of 500 with a frozen clock, want exactly %d", got, capacity)
	}
}
