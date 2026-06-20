package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStore_SetIfAbsentAndCAS(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("fresh key should be absent")
	}
	if ok, _ := s.SetIfAbsent(ctx, "k", 100, time.Minute); !ok {
		t.Fatal("SetIfAbsent should succeed on an absent key")
	}
	if ok, _ := s.SetIfAbsent(ctx, "k", 200, time.Minute); ok {
		t.Fatal("SetIfAbsent should fail on a present key")
	}
	if v, ok, _ := s.Get(ctx, "k"); !ok || v != 100 {
		t.Fatalf("Get = %d,%v want 100,true", v, ok)
	}
	if ok, _ := s.CompareAndSwap(ctx, "k", 999, 300, time.Minute); ok {
		t.Fatal("CAS with the wrong old value should fail")
	}
	if ok, _ := s.CompareAndSwap(ctx, "k", 100, 300, time.Minute); !ok {
		t.Fatal("CAS with the right old value should succeed")
	}
	if v, _, _ := s.Get(ctx, "k"); v != 300 {
		t.Fatalf("after CAS Get = %d, want 300", v)
	}
}

func TestMemoryStore_TTLExpiry(t *testing.T) {
	s := NewMemoryStore()
	clk := newFakeClock(time.Unix(1000, 0))
	s.now = clk.now
	ctx := context.Background()

	s.SetIfAbsent(ctx, "k", 1, time.Second)
	if _, ok, _ := s.Get(ctx, "k"); !ok {
		t.Fatal("should exist before expiry")
	}
	clk.advance(time.Second) // now == expiresAt → expired
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("should be expired once now reaches expiresAt")
	}
	// CAS on an expired key behaves like CAS on an absent key.
	if ok, _ := s.CompareAndSwap(ctx, "k", 1, 2, time.Second); ok {
		t.Fatal("CAS on an expired key should fail")
	}
	if ok, _ := s.SetIfAbsent(ctx, "k", 2, time.Second); !ok {
		t.Fatal("SetIfAbsent should succeed after expiry")
	}
}

func TestMemoryStore_Janitor(t *testing.T) {
	s := NewMemoryStore(WithCleanupInterval(2 * time.Millisecond))
	defer s.Close()
	ctx := context.Background()
	s.SetIfAbsent(ctx, "k", 1, time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.m)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("janitor did not evict the expired entry")
		}
		time.Sleep(time.Millisecond)
	}
}
