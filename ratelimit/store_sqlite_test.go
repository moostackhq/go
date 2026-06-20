package ratelimit

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // :memory: is per-connection; keep one
	t.Cleanup(func() { db.Close() })
	s, err := NewSQLiteStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLiteStore_SetIfAbsentAndCAS(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("fresh key should be absent")
	}
	if ok, err := s.SetIfAbsent(ctx, "k", 100, time.Minute); err != nil || !ok {
		t.Fatalf("SetIfAbsent on absent = %v,%v want true,nil", ok, err)
	}
	if ok, _ := s.SetIfAbsent(ctx, "k", 200, time.Minute); ok {
		t.Fatal("SetIfAbsent on present should fail")
	}
	if v, ok, _ := s.Get(ctx, "k"); !ok || v != 100 {
		t.Fatalf("Get = %d,%v want 100,true", v, ok)
	}
	if ok, _ := s.CompareAndSwap(ctx, "k", 999, 300, time.Minute); ok {
		t.Fatal("CAS with wrong old should fail")
	}
	if ok, err := s.CompareAndSwap(ctx, "k", 100, 300, time.Minute); err != nil || !ok {
		t.Fatalf("CAS with right old = %v,%v want true,nil", ok, err)
	}
	if v, _, _ := s.Get(ctx, "k"); v != 300 {
		t.Fatalf("after CAS Get = %d, want 300", v)
	}
	// CAS on an absent key fails.
	if ok, _ := s.CompareAndSwap(ctx, "missing", 0, 1, time.Minute); ok {
		t.Fatal("CAS on absent key should fail")
	}
}

func TestSQLiteStore_TTLExpiry(t *testing.T) {
	s := newSQLiteStore(t)
	clk := newFakeClock(time.Unix(1000, 0))
	s.now = clk.now
	ctx := context.Background()

	s.SetIfAbsent(ctx, "k", 1, time.Second)
	if _, ok, _ := s.Get(ctx, "k"); !ok {
		t.Fatal("should exist before expiry")
	}
	clk.advance(time.Second) // now == expires_at → expired
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("should be expired once now reaches expires_at")
	}
	// CAS on an expired key behaves like CAS on an absent key.
	if ok, _ := s.CompareAndSwap(ctx, "k", 1, 2, time.Second); ok {
		t.Fatal("CAS on expired key should fail")
	}
	// SetIfAbsent overwrites an expired row.
	if ok, _ := s.SetIfAbsent(ctx, "k", 2, time.Second); !ok {
		t.Fatal("SetIfAbsent should succeed over an expired row")
	}
	if v, _, _ := s.Get(ctx, "k"); v != 2 {
		t.Fatalf("after re-set Get = %d, want 2", v)
	}
}

func TestSQLiteStore_Sweep(t *testing.T) {
	s := newSQLiteStore(t)
	clk := newFakeClock(time.Unix(1000, 0))
	s.now = clk.now
	ctx := context.Background()

	s.SetIfAbsent(ctx, "a", 1, time.Second)
	s.SetIfAbsent(ctx, "b", 1, time.Hour)
	clk.advance(time.Second) // "a" now expired, "b" still live
	n, err := s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Sweep removed %d, want 1", n)
	}
	if _, ok, _ := s.Get(ctx, "b"); !ok {
		t.Error("live key b should survive sweep")
	}
}

// TestSQLiteStore_LimiterNoOverAdmission runs the full limiter over the
// SQLite backend with a frozen clock and concurrent callers using the
// DEFAULT retry budget: the cross-statement CAS loop must admit
// exactly the capacity. Run under -race.
func TestSQLiteStore_LimiterNoOverAdmission(t *testing.T) {
	s := newSQLiteStore(t)
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	s.now = clk.now
	const capacity = 30
	lim, _ := New(s, Limit{Rate: 1, Period: time.Second, Burst: capacity}, WithClock(clk.now))
	ctx := context.Background()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
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
		t.Errorf("allowed %d of 200 with a frozen clock, want exactly %d", got, capacity)
	}
}

func TestSQLiteStore_WithoutSchema(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	// WithoutSchema does not create the table, so ops error until it
	// exists.
	s, err := NewSQLiteStore(ctx, db, WithoutSchema())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "k"); err == nil {
		t.Fatal("Get without a table should error")
	}

	// Once the caller (or a migration) creates it, the store works.
	if _, err := db.ExecContext(ctx, `CREATE TABLE rate_limits (
		key TEXT PRIMARY KEY, value INTEGER NOT NULL, expires_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetIfAbsent(ctx, "k", 1, time.Minute); err != nil || !ok {
		t.Fatalf("after creating table, SetIfAbsent = %v,%v want true,nil", ok, err)
	}
}

func TestNewSQLiteStore_InvalidTable(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	t.Cleanup(func() { db.Close() })
	if _, err := NewSQLiteStore(context.Background(), db, WithTable("bad name; DROP")); err == nil {
		t.Fatal("expected error for invalid table name")
	}
}
