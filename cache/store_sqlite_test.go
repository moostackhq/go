package cache

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newSQLiteStore(t *testing.T, opts ...SQLiteOption) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // :memory: is per-connection; keep one
	t.Cleanup(func() { db.Close() })
	s, err := NewSQLiteStore(context.Background(), db, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLiteStore_SetGetDelete(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("fresh key should be absent")
	}
	if err := s.Set(ctx, "k", []byte("v1"), 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Get(ctx, "k")
	if err != nil || !ok || string(v) != "v1" {
		t.Fatalf("get = %q, %v, %v", v, ok, err)
	}
	// Set overwrites.
	if err := s.Set(ctx, "k", []byte("v2"), 0); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Get(ctx, "k"); string(v) != "v2" {
		t.Errorf("Set should overwrite, got %q", v)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("should be absent after delete")
	}
	if err := s.Delete(ctx, "absent"); err != nil {
		t.Fatalf("deleting absent key should be a no-op, got %v", err)
	}
}

func TestSQLiteStore_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newSQLiteStore(t)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	_ = s.Set(ctx, "k", []byte("v"), time.Minute) // expires at 1060
	if _, ok, _ := s.Get(ctx, "k"); !ok {
		t.Fatal("should be present before expiry")
	}
	now = now.Add(time.Minute) // exactly at expiry → filtered out
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("should read as absent at the TTL boundary")
	}
	// ttl <= 0 never expires.
	_ = s.Set(ctx, "forever", []byte("v"), 0)
	now = now.Add(100 * time.Hour)
	if _, ok, _ := s.Get(ctx, "forever"); !ok {
		t.Fatal("ttl<=0 should never expire")
	}
}

func TestSQLiteStore_Sweep(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newSQLiteStore(t)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	_ = s.Set(ctx, "short", []byte("v"), time.Minute) // expires at 1060
	_ = s.Set(ctx, "forever", []byte("v"), 0)
	now = now.Add(2 * time.Minute) // short is now expired

	n, err := s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Sweep removed %d rows, want 1 (only the expired one)", n)
	}
	if _, ok, _ := s.Get(ctx, "forever"); !ok {
		t.Error("the never-expiring row should survive Sweep")
	}
}

func TestSQLiteStore_WithoutSchemaRequiresTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	s, err := NewSQLiteStore(context.Background(), db, WithoutSchema())
	if err != nil {
		t.Fatal(err)
	}
	// No table was created, so a query should error rather than panic.
	if _, _, err := s.Get(context.Background(), "k"); err == nil {
		t.Error("expected an error querying a missing table")
	}
}

func TestSQLiteStore_InvalidTableName(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	t.Cleanup(func() { db.Close() })
	if _, err := NewSQLiteStore(context.Background(), db, WithTable("bad name; DROP")); err == nil {
		t.Error("expected an invalid-table-name error")
	}
}

func TestSQLiteStore_AsCacheBackend(t *testing.T) {
	s := newSQLiteStore(t, WithTable("rollups"))
	c := New[map[string]int](s, WithNamespace("daily"))
	ctx := context.Background()

	var calls int
	load := func(context.Context) (map[string]int, error) {
		calls++
		return map[string]int{"up": 7}, nil
	}
	for i := 0; i < 3; i++ {
		v, err := c.GetOrLoad(ctx, "2026-06-19", time.Minute, load)
		if err != nil || v["up"] != 7 {
			t.Fatalf("GetOrLoad = %v, %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("load ran %d times through the SQLite backend, want 1", calls)
	}
}
