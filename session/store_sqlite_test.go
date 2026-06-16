package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newSQLiteStore(t *testing.T) *SQLiteStore[cart] {
	t.Helper()
	s, err := NewSQLiteStore[cart](openSQLite(t), JSONCodec[cart]{}, SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLiteSchema_NotEmpty(t *testing.T) {
	if SQLiteSchema() == "" {
		t.Fatal("SQLiteSchema returned empty string")
	}
}

func TestSQLiteStore_AutoCreateIsIdempotent(t *testing.T) {
	db := openSQLite(t)
	for i := range 3 {
		if _, err := NewSQLiteStore[cart](db, JSONCodec[cart]{}, SQLiteOptions{AutoCreate: true}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
}

func TestSQLiteStore_ManualSchema(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec(SQLiteSchema()); err != nil {
		t.Fatalf("apply schema manually: %v", err)
	}
	s, err := NewSQLiteStore[cart](db, JSONCodec[cart]{}, SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(context.Background(), Record[cart]{
		SID:            "abc",
		IdleExpiry:     time.Now().Add(time.Hour),
		AbsoluteExpiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save against manually-applied schema: %v", err)
	}
}

func TestSQLiteStore_SaveLoadRoundtrip(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	in := Record[cart]{
		SID:            "abc",
		UserID:         "user-1",
		AbsoluteExpiry: time.Now().Add(time.Hour).Round(time.Microsecond),
		IdleExpiry:     time.Now().Add(time.Hour).Round(time.Microsecond),
		Payload:        cart{Items: []string{"a", "b"}},
	}
	stored, err := s.Save(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 {
		t.Errorf("first save version: want 1, got %d", stored.Version)
	}

	got, err := s.Load(ctx, "abc")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.UserID != in.UserID {
		t.Errorf("user_id: want %q, got %q", in.UserID, got.UserID)
	}
	if len(got.Payload.Items) != 2 || got.Payload.Items[1] != "b" {
		t.Errorf("payload mismatch: %+v", got.Payload)
	}
	if !got.AbsoluteExpiry.Equal(in.AbsoluteExpiry) {
		t.Errorf("absolute_expiry: want %v, got %v", in.AbsoluteExpiry, got.AbsoluteExpiry)
	}
}

func TestSQLiteStore_CASConflict_OnUpdate(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	base := Record[cart]{SID: "abc", IdleExpiry: time.Now().Add(time.Hour)}
	v1, err := s.Save(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	// Another writer still has version 0 in hand.
	if _, err := s.Save(ctx, base); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale insert: want ErrVersionConflict, got %v", err)
	}

	// Correct version writes through.
	if _, err := s.Save(ctx, v1); err != nil {
		t.Fatalf("fresh update: %v", err)
	}
}

func TestSQLiteStore_CASConflict_OnUnknownSIDWithVersion(t *testing.T) {
	s := newSQLiteStore(t)
	rec := Record[cart]{SID: "abc", Version: 5, IdleExpiry: time.Now().Add(time.Hour)}
	if _, err := s.Save(context.Background(), rec); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("phantom update: want ErrVersionConflict, got %v", err)
	}
}

func TestSQLiteStore_LoadMissingAndExpired(t *testing.T) {
	s := newSQLiteStore(t)
	clock := time.Now()
	s.now = func() time.Time { return clock }

	if _, err := s.Load(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing load: want ErrNotFound, got %v", err)
	}

	if _, err := s.Save(context.Background(), Record[cart]{
		SID:            "exp",
		AbsoluteExpiry: clock.Add(time.Minute),
		IdleExpiry:     clock.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := s.Load(context.Background(), "exp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired load: want ErrNotFound, got %v", err)
	}
}

func TestSQLiteStore_BumpTTL_RejectsExpired(t *testing.T) {
	s := newSQLiteStore(t)
	clock := time.Now()
	s.now = func() time.Time { return clock }

	if _, err := s.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: clock.Add(time.Minute),
		IdleExpiry:     clock.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// Advance past expiry but skip Sweep — the row still exists.
	clock = clock.Add(2 * time.Minute)

	if err := s.BumpTTL(context.Background(), "abc", clock.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BumpTTL on expired row: want ErrNotFound, got %v", err)
	}
	// And the row should still be unfindable via Load.
	if _, err := s.Load(context.Background(), "abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after rejected bump: want ErrNotFound, got %v", err)
	}
}

func TestSQLiteStore_BumpTTLDoesNotConflictWithCAS(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	rec := Record[cart]{SID: "abc", IdleExpiry: time.Now().Add(time.Minute)}
	v1, err := s.Save(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}

	newExp := time.Now().Add(2 * time.Hour)
	if err := s.BumpTTL(ctx, "abc", newExp); err != nil {
		t.Fatal(err)
	}

	// The CAS path still works on the version we held BEFORE the
	// bump — bumping must not bump version.
	if _, err := s.Save(ctx, v1); err != nil {
		t.Errorf("save after bump: want success, got %v", err)
	}

	if err := s.BumpTTL(ctx, "missing", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("bump missing: want ErrNotFound, got %v", err)
	}
}

func TestSQLiteStore_UserIndex(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	mk := func(sid, uid string) Record[cart] {
		return Record[cart]{SID: sid, UserID: uid, IdleExpiry: time.Now().Add(time.Hour)}
	}
	for _, r := range []Record[cart]{mk("a", "u1"), mk("b", "u1"), mk("c", "u2")} {
		if _, err := s.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListByUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("u1 list: want 2, got %d (%v)", len(got), got)
	}

	n, err := s.RevokeByUser(ctx, "u1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("revoked count: want 1, got %d", n)
	}
	if _, err := s.Load(ctx, "a"); err != nil {
		t.Errorf("a should still exist: %v", err)
	}
	if _, err := s.Load(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("b should be gone: %v", err)
	}
}

func TestSQLiteStore_Scan(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	for _, sid := range []string{"a", "b", "c"} {
		if _, err := s.Save(ctx, Record[cart]{SID: sid, IdleExpiry: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	if err := s.Scan(ctx, func(sid string) bool { seen = append(seen, sid); return true }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Errorf("scan count: want 3, got %d", len(seen))
	}

	// Early stop honored.
	var partial int
	s.Scan(ctx, func(string) bool { partial++; return false })
	if partial != 1 {
		t.Errorf("early-stop scan: want 1 call, got %d", partial)
	}
}

func TestSQLiteStore_Sweep(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()
	clock := time.Now()
	s.now = func() time.Time { return clock }

	// One live, one expired by absolute, one expired by idle.
	if _, err := s.Save(ctx, Record[cart]{SID: "live", AbsoluteExpiry: clock.Add(time.Hour), IdleExpiry: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, Record[cart]{SID: "abs-dead", AbsoluteExpiry: clock.Add(-time.Hour), IdleExpiry: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, Record[cart]{SID: "idle-dead", AbsoluteExpiry: clock.Add(time.Hour), IdleExpiry: clock.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	n, err := s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("sweep count: want 2, got %d", n)
	}

	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rows)
	if rows != 1 {
		t.Errorf("after sweep: want 1 row, got %d", rows)
	}
}

func TestSQLiteStore_PayloadIsJSON(t *testing.T) {
	s := newSQLiteStore(t)
	if _, err := s.Save(context.Background(), Record[cart]{
		SID:        "abc",
		IdleExpiry: time.Now().Add(time.Hour),
		Payload:    cart{Items: []string{"hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.db.QueryRow(`SELECT payload FROM sessions WHERE sid = 'abc'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var decoded cart
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload column is not valid JSON: %v (%s)", err, raw)
	}
	if len(decoded.Items) != 1 || decoded.Items[0] != "hello" {
		t.Errorf("decoded payload: %+v", decoded)
	}
}

// brokenCodec encodes anything to a fixed byte sequence and always
// fails to decode — simulates a payload type that has changed shape
// in an incompatible way since the row was written.
type brokenCodec struct{}

func (brokenCodec) Encode(cart) ([]byte, error) { return []byte(`{"items":["x"]}`), nil }
func (brokenCodec) Decode([]byte) (cart, error) { return cart{}, errors.New("decoder rejects payload") }

func TestSQLiteStore_DecodeFailure_GracefulByDefault(t *testing.T) {
	db := openSQLite(t)
	// Seed via a working codec.
	writer, _ := NewSQLiteStore[cart](db, JSONCodec[cart]{}, SQLiteOptions{AutoCreate: true})
	if _, err := writer.Save(context.Background(), Record[cart]{
		SID:        "abc",
		IdleExpiry: time.Now().Add(time.Hour),
		Payload:    cart{Items: []string{"hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Read via a codec that can no longer parse the on-disk shape.
	reader, _ := NewSQLiteStore[cart](db, brokenCodec{}, SQLiteOptions{})
	if _, err := reader.Load(context.Background(), "abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("graceful mode: want ErrNotFound, got %v", err)
	}
}

func TestSQLiteStore_DecodeFailure_StrictPropagates(t *testing.T) {
	db := openSQLite(t)
	writer, _ := NewSQLiteStore[cart](db, JSONCodec[cart]{}, SQLiteOptions{AutoCreate: true})
	writer.Save(context.Background(), Record[cart]{
		SID:        "abc",
		IdleExpiry: time.Now().Add(time.Hour),
		Payload:    cart{Items: []string{"hello"}},
	})
	reader, _ := NewSQLiteStore[cart](db, brokenCodec{}, SQLiteOptions{StrictDecode: true})
	_, err := reader.Load(context.Background(), "abc")
	if err == nil {
		t.Fatal("strict mode: want error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("strict mode: error should NOT be ErrNotFound, got %v", err)
	}
}

func TestSQLiteStore_ImplementsOptionalInterfaces(t *testing.T) {
	var s any = newSQLiteStore(t)
	if _, ok := s.(TTLBumper); !ok {
		t.Error("SQLiteStore should implement TTLBumper")
	}
	if _, ok := s.(UserIndexer); !ok {
		t.Error("SQLiteStore should implement UserIndexer")
	}
	if _, ok := s.(Scanner); !ok {
		t.Error("SQLiteStore should implement Scanner")
	}
	if _, ok := s.(Sweeper); !ok {
		t.Error("SQLiteStore should implement Sweeper")
	}
}

func TestSQLiteStore_ConcurrentUpdateThroughManager(t *testing.T) {
	store := newSQLiteStore(t)
	mgr, err := New[cart](Config[cart]{
		Store:          store,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
		MaxRetries:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a session so all goroutines hit the same SID.
	seed, _ := store.Save(context.Background(), Record[cart]{
		SID:            "shared",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})

	const writers = 8
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := &state[cart]{sid: seed.SID}
			ctx := context.WithValue(context.Background(), mgr.ctxKey, st)
			if err := mgr.Update(ctx, func(c *cart) error {
				c.Items = append(c.Items, "x")
				return nil
			}); err != nil {
				failures.Add(1)
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() > 0 {
		return
	}
	got, err := store.Load(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Payload.Items) != writers {
		t.Errorf("merged items: want %d, got %d (%v)", writers, len(got.Payload.Items), got.Payload.Items)
	}
}
