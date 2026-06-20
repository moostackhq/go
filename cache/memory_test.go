package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMemory_SetGetDelete(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("expected miss on empty store")
	}
	if err := m.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := m.Get(ctx, "k")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("get = %q, %v, %v", v, ok, err)
	}
	if err := m.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("expected miss after delete")
	}
	if err := m.Delete(ctx, "absent"); err != nil {
		t.Fatalf("deleting absent key should be a no-op, got %v", err)
	}
}

func TestMemory_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	m := NewMemory()
	m.now = func() time.Time { return now }
	ctx := context.Background()

	_ = m.Set(ctx, "k", []byte("v"), time.Minute)
	if _, ok, _ := m.Get(ctx, "k"); !ok {
		t.Fatal("should be present before expiry")
	}
	now = now.Add(time.Minute) // exactly at expiry → expired
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("should be expired at the TTL boundary")
	}
	// A zero/negative TTL never expires.
	_ = m.Set(ctx, "forever", []byte("v"), 0)
	now = now.Add(100 * time.Hour)
	if _, ok, _ := m.Get(ctx, "forever"); !ok {
		t.Fatal("ttl<=0 should never expire")
	}
}

func TestMemory_LRUEviction(t *testing.T) {
	m := NewMemory(WithMaxEntries(2))
	ctx := context.Background()

	_ = m.Set(ctx, "a", []byte("1"), 0)
	_ = m.Set(ctx, "b", []byte("2"), 0)
	_, _, _ = m.Get(ctx, "a") // touch a so b is the LRU
	_ = m.Set(ctx, "c", []byte("3"), 0)

	if _, ok, _ := m.Get(ctx, "b"); ok {
		t.Error("b should have been evicted as least-recently-used")
	}
	if _, ok, _ := m.Get(ctx, "a"); !ok {
		t.Error("a should survive")
	}
	if _, ok, _ := m.Get(ctx, "c"); !ok {
		t.Error("c should be present")
	}
}

func TestMemory_GetReturnsCopy(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("orig"), 0)

	v, _, _ := m.Get(ctx, "k")
	v[0] = 'X' // mutate the returned slice
	again, _, _ := m.Get(ctx, "k")
	if string(again) != "orig" {
		t.Errorf("stored bytes were mutated through the returned slice: %q", again)
	}
}

func TestMemory_SetCopiesInput(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	in := []byte("orig")
	_ = m.Set(ctx, "k", in, 0)
	in[0] = 'X' // mutate the caller's slice after Set

	v, _, _ := m.Get(ctx, "k")
	if string(v) != "orig" {
		t.Errorf("stored bytes alias the caller's slice: %q", v)
	}
}

func TestMemory_JanitorReaps(t *testing.T) {
	now := time.Unix(1000, 0)
	m := NewMemory()
	m.now = func() time.Time { return now }
	ctx := context.Background()

	_ = m.Set(ctx, "k", []byte("v"), time.Minute)
	now = now.Add(2 * time.Minute)
	m.reapExpired() // invoke the sweep directly, deterministically

	m.mu.Lock()
	n := len(m.items)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("janitor should have reaped the expired entry, %d left", n)
	}
}

func TestMemory_LRUExpiredFreesSlot(t *testing.T) {
	now := time.Unix(1000, 0)
	m := NewMemory(WithMaxEntries(2))
	m.now = func() time.Time { return now }
	ctx := context.Background()

	_ = m.Set(ctx, "a", []byte("1"), time.Minute) // expires at 1060
	_ = m.Set(ctx, "b", []byte("2"), 0)           // never expires
	now = now.Add(2 * time.Minute)                // a is now expired

	if _, ok, _ := m.Get(ctx, "a"); ok { // lazy expiry removes a, freeing its slot
		t.Fatal("a should be expired")
	}
	_ = m.Set(ctx, "c", []byte("3"), 0) // fits without evicting b
	if _, ok, _ := m.Get(ctx, "b"); !ok {
		t.Error("b should survive — a's expiry freed the slot, so no eviction was needed")
	}
	if _, ok, _ := m.Get(ctx, "c"); !ok {
		t.Error("c should be present")
	}
}

func TestMemory_JanitorGoroutineReaps(t *testing.T) {
	m := NewMemory(WithJanitor(2 * time.Millisecond))
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("v"), 5*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.items)
		m.mu.Unlock()
		if n == 0 {
			return // the live janitor goroutine reclaimed it
		}
		if time.Now().After(deadline) {
			t.Fatal("janitor did not reap the expired entry within 2s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestMemory_IgnoresContext(t *testing.T) {
	m := NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — the in-process store ignores it
	if err := m.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set under cancelled ctx: %v", err)
	}
	if _, ok, err := m.Get(ctx, "k"); err != nil || !ok {
		t.Fatalf("Get under cancelled ctx: ok=%v err=%v", ok, err)
	}
}

func TestMemory_ConcurrentAccess(t *testing.T) {
	m := NewMemory(WithMaxEntries(64))
	ctx := context.Background()
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				k := fmt.Sprintf("k%d", j%32)
				switch j % 3 {
				case 0:
					_ = m.Set(ctx, k, []byte{byte(id)}, time.Minute)
				case 1:
					_, _, _ = m.Get(ctx, k)
				default:
					_ = m.Delete(ctx, k)
				}
			}
		}(g)
	}
	wg.Wait() // the -race detector is the real assertion here
}

func TestMemory_CloseIdempotent(t *testing.T) {
	m := NewMemory(WithJanitor(time.Hour))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close should be safe, got %v", err)
	}
	// Close on a janitor-less store is also fine.
	if err := NewMemory().Close(); err != nil {
		t.Fatal(err)
	}
}
