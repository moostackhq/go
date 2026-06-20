package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/cache"
)

type rollup struct {
	Total int
	Label string
}

func TestCache_SetGetRoundTrip(t *testing.T) {
	c := cache.New[rollup](cache.NewMemory())
	ctx := context.Background()

	if _, ok, err := c.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("expected clean miss, got ok=%v err=%v", ok, err)
	}
	want := rollup{Total: 7, Label: "prod"}
	if err := c.Set(ctx, "k", want, 0); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || got != want {
		t.Fatalf("get = %+v, ok=%v, err=%v; want %+v", got, ok, err, want)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("expected miss after delete")
	}
}

func TestCache_NamespaceIsolation(t *testing.T) {
	store := cache.NewMemory()
	a := cache.New[rollup](store, cache.WithNamespace("a"))
	b := cache.New[rollup](store, cache.WithNamespace("b"))
	ctx := context.Background()

	_ = a.Set(ctx, "k", rollup{Total: 1}, 0)
	_ = b.Set(ctx, "k", rollup{Total: 2}, 0)

	av, _, _ := a.Get(ctx, "k")
	bv, _, _ := b.Get(ctx, "k")
	if av.Total != 1 || bv.Total != 2 {
		t.Errorf("namespaces collided: a=%d b=%d", av.Total, bv.Total)
	}
}

func TestCache_GetOrLoad_LoadsOnMissCachesResult(t *testing.T) {
	c := cache.New[rollup](cache.NewMemory())
	ctx := context.Background()

	var calls int32
	load := func(context.Context) (rollup, error) {
		atomic.AddInt32(&calls, 1)
		return rollup{Total: 42}, nil
	}
	for i := 0; i < 3; i++ {
		v, err := c.GetOrLoad(ctx, "k", time.Minute, load)
		if err != nil || v.Total != 42 {
			t.Fatalf("GetOrLoad = %+v, %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("load ran %d times, want 1 (subsequent calls are cache hits)", calls)
	}
}

func TestCache_GetOrLoad_SingleFlight(t *testing.T) {
	c := cache.New[rollup](cache.NewMemory())
	ctx := context.Background()

	var calls int32
	release := make(chan struct{})
	load := func(context.Context) (rollup, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the leader so the others pile up behind it
		return rollup{Total: 1}, nil
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := c.GetOrLoad(ctx, "k", time.Minute, load); err != nil {
				t.Errorf("GetOrLoad: %v", err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the goroutines reach the flight
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Errorf("load ran %d times under concurrency, want 1 (single-flight)", calls)
	}
}

func TestCache_GetOrLoad_ErrorNotCached(t *testing.T) {
	c := cache.New[rollup](cache.NewMemory())
	ctx := context.Background()
	boom := errors.New("boom")

	var calls int32
	load := func(context.Context) (rollup, error) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&calls) == 1 {
			return rollup{}, boom
		}
		return rollup{Total: 9}, nil
	}

	if _, err := c.GetOrLoad(ctx, "k", time.Minute, load); !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	// The failed load must not have been cached; a second call retries.
	v, err := c.GetOrLoad(ctx, "k", time.Minute, load)
	if err != nil || v.Total != 9 {
		t.Fatalf("retry after error = %+v, %v", v, err)
	}
	if calls != 2 {
		t.Errorf("load ran %d times, want 2", calls)
	}
}

// failingStore lets a Get error exercise the best-effort read path.
type failingStore struct{ cache.Store }

func (failingStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("backend down")
}

// setFailStore fails every write while reads/deletes pass through.
type setFailStore struct{ *cache.Memory }

func (setFailStore) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("write failed")
}

func TestCache_GetOrLoad_ReadErrorTreatedAsMiss(t *testing.T) {
	c := cache.New[rollup](failingStore{Store: cache.NewMemory()})
	ctx := context.Background()

	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (rollup, error) {
		return rollup{Total: 5}, nil
	})
	if err != nil || v.Total != 5 {
		t.Fatalf("read error should fall through to load: got %+v, %v", v, err)
	}
}

func TestCache_GetOrLoad_LoadPanicPropagatesToAllCallers(t *testing.T) {
	c := cache.New[rollup](cache.NewMemory())
	ctx := context.Background()

	release := make(chan struct{})
	var calls int32
	load := func(context.Context) (rollup, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the leader so the rest attach as waiters
		panic("load blew up")
	}

	const n = 8
	var wg sync.WaitGroup
	var panicked int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt32(&panicked, 1)
					if r != "load blew up" {
						t.Errorf("recovered %v, want the original panic value", r)
					}
				}
			}()
			_, _ = c.GetOrLoad(ctx, "k", time.Minute, load)
		}()
	}
	time.Sleep(20 * time.Millisecond) // let all goroutines reach the flight
	close(release)
	wg.Wait()

	// load ran once proves single-flight held: the other 7 were waiters
	// that re-raised the leader's panic, not second leaders that panicked
	// on their own.
	if calls != 1 {
		t.Errorf("load ran %d times, want 1 (waiters must not become second leaders)", calls)
	}
	if panicked != n {
		t.Errorf("%d of %d callers panicked, want all (leader + waiters re-raise)", panicked, n)
	}
}

func TestCache_ErrorHook_SetFailureReported(t *testing.T) {
	var mu sync.Mutex
	var ops []string
	c := cache.New[rollup](setFailStore{cache.NewMemory()}, cache.WithErrorHook(
		func(op, key string, err error) {
			mu.Lock()
			ops = append(ops, op)
			mu.Unlock()
		}))
	ctx := context.Background()

	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (rollup, error) {
		return rollup{Total: 3}, nil
	})
	if err != nil || v.Total != 3 {
		t.Fatalf("value should be returned despite the Set failure: %+v %v", v, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ops) != 1 || ops[0] != "set" {
		t.Errorf("expected one 'set' fault reported, got %v", ops)
	}
}

func TestCache_ErrorHook_ReadFailureReportedOncePerCall(t *testing.T) {
	var ops []string
	c := cache.New[rollup](failingStore{Store: cache.NewMemory()}, cache.WithErrorHook(
		func(op, key string, err error) { ops = append(ops, op) }))
	// A single (uncontended) call must report the read fault once — the
	// inner single-flight re-check hits the same broken backend but must
	// not report a duplicate.
	_, _ = c.GetOrLoad(context.Background(), "k", time.Minute, func(context.Context) (rollup, error) {
		return rollup{Total: 1}, nil
	})
	if len(ops) != 1 || ops[0] != "get" {
		t.Errorf("expected exactly one 'get' fault, got %v", ops)
	}
}

func TestCache_LargeValueRoundTrip(t *testing.T) {
	c := cache.New[[]byte](cache.NewMemory())
	ctx := context.Background()
	big := make([]byte, 1<<20) // 1 MiB
	for i := range big {
		big[i] = byte(i)
	}
	if err := c.Set(ctx, "k", big, 0); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || len(got) != len(big) || got[1000] != big[1000] {
		t.Fatalf("large value roundtrip failed: ok=%v err=%v len=%d", ok, err, len(got))
	}
}

// upperCodec is a trivial non-JSON codec to prove WithCodec is honored.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error)   { return []byte(v.(string)), nil }
func (rawCodec) Unmarshal(b []byte, v any) error { *v.(*string) = string(b); return nil }

func TestCache_WithCodec(t *testing.T) {
	c := cache.New[string](cache.NewMemory(), cache.WithCodec(rawCodec{}))
	ctx := context.Background()
	if err := c.Set(ctx, "k", "hello", 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || v != "hello" {
		t.Fatalf("get = %q, %v, %v", v, ok, err)
	}
}
