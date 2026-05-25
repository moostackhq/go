package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

type cart struct {
	Items []string
}

func TestMemoryStore_SaveLoadDelete(t *testing.T) {
	s := NewMemoryStore[cart]()
	ctx := context.Background()

	if _, err := s.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing: want ErrNotFound, got %v", err)
	}

	stored, err := s.Save(ctx, Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
		Payload:        cart{Items: []string{"x"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if stored.Version != 1 {
		t.Errorf("first save version: want 1, got %d", stored.Version)
	}

	got, err := s.Load(ctx, "abc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Payload.Items) != 1 || got.Payload.Items[0] != "x" {
		t.Errorf("payload roundtrip: got %+v", got.Payload)
	}

	if err := s.Delete(ctx, "abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, "abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after Delete: want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_CASConflict(t *testing.T) {
	s := NewMemoryStore[cart]()
	ctx := context.Background()

	base := Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	}
	v1, err := s.Save(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	// Second writer also loaded v0 and tries to save again.
	if _, err := s.Save(ctx, base); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale write: want ErrVersionConflict, got %v", err)
	}

	// Correct version succeeds.
	if _, err := s.Save(ctx, v1); err != nil {
		t.Fatalf("fresh write: %v", err)
	}
}

func TestMemoryStore_CASOnUnknownSID(t *testing.T) {
	s := NewMemoryStore[cart]()
	rec := Record[cart]{SID: "abc", Version: 5, IdleExpiry: time.Now().Add(time.Hour)}
	if _, err := s.Save(context.Background(), rec); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("nonzero version on unknown sid: want ErrVersionConflict, got %v", err)
	}
}

func TestMemoryStore_LoadExpired(t *testing.T) {
	s := NewMemoryStore[cart]()
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }

	if _, err := s.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: clock.Add(time.Minute),
		IdleExpiry:     clock.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := s.Load(context.Background(), "abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired load: want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_ListByUser_FiltersExpired(t *testing.T) {
	s := NewMemoryStore[cart]()
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }
	ctx := context.Background()

	s.Save(ctx, Record[cart]{SID: "live", UserID: "u1",
		AbsoluteExpiry: clock.Add(time.Hour), IdleExpiry: clock.Add(time.Hour)})
	s.Save(ctx, Record[cart]{SID: "dead", UserID: "u1",
		AbsoluteExpiry: clock.Add(time.Minute), IdleExpiry: clock.Add(time.Minute)})

	clock = clock.Add(2 * time.Minute) // "dead" is now expired but still indexed

	got, err := s.ListByUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "live" {
		t.Errorf("expected only live session, got %v", got)
	}
}

func TestMemoryStore_Scan_FiltersExpired(t *testing.T) {
	s := NewMemoryStore[cart]()
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }
	ctx := context.Background()

	s.Save(ctx, Record[cart]{SID: "live", IdleExpiry: clock.Add(time.Hour)})
	s.Save(ctx, Record[cart]{SID: "dead", IdleExpiry: clock.Add(time.Minute)})

	clock = clock.Add(2 * time.Minute)

	var seen []string
	s.Scan(ctx, func(sid string) bool { seen = append(seen, sid); return true })
	if len(seen) != 1 || seen[0] != "live" {
		t.Errorf("Scan should skip expired rows, got %v", seen)
	}
}

func TestMemoryStore_BumpTTL_RejectsExpired(t *testing.T) {
	s := NewMemoryStore[cart]()
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }

	if _, err := s.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: clock.Add(time.Minute),
		IdleExpiry:     clock.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// Advance past expiry — the record is still in the map (no
	// sweep) but Load and BumpTTL should both treat it as gone.
	clock = clock.Add(2 * time.Minute)
	if err := s.BumpTTL(context.Background(), "abc", clock.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("BumpTTL on expired record: want ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_UserIndex(t *testing.T) {
	s := NewMemoryStore[cart]()
	ctx := context.Background()
	mkRec := func(sid, uid string) Record[cart] {
		return Record[cart]{
			SID:            sid,
			UserID:         uid,
			AbsoluteExpiry: time.Now().Add(time.Hour),
			IdleExpiry:     time.Now().Add(time.Hour),
		}
	}
	for _, r := range []Record[cart]{mkRec("a", "u1"), mkRec("b", "u1"), mkRec("c", "u2")} {
		if _, err := s.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	u1, _ := s.ListByUser(ctx, "u1")
	if len(u1) != 2 {
		t.Errorf("u1 list: want 2, got %d (%v)", len(u1), u1)
	}

	revoked, err := s.RevokeByUser(ctx, "u1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Errorf("revoked count: want 1, got %d", revoked)
	}

	// "a" survives; "b" gone.
	if _, err := s.Load(ctx, "a"); err != nil {
		t.Errorf("a should still exist: %v", err)
	}
	if _, err := s.Load(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("b should be gone: %v", err)
	}
}

func TestMemoryStore_UserIndexFollowsChanges(t *testing.T) {
	s := NewMemoryStore[cart]()
	ctx := context.Background()
	rec := Record[cart]{
		SID:            "a",
		UserID:         "u1",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	}
	v1, err := s.Save(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	v1.UserID = "u2"
	if _, err := s.Save(ctx, v1); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.ListByUser(ctx, "u1"); len(got) != 0 {
		t.Errorf("u1 after reassign: want 0, got %v", got)
	}
	if got, _ := s.ListByUser(ctx, "u2"); len(got) != 1 || got[0] != "a" {
		t.Errorf("u2 after reassign: want [a], got %v", got)
	}
}

func TestMemoryStore_Sweep(t *testing.T) {
	s := NewMemoryStore[cart]()
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }
	ctx := context.Background()

	mk := func(sid string, abs, idle time.Time) {
		if _, err := s.Save(ctx, Record[cart]{SID: sid, AbsoluteExpiry: abs, IdleExpiry: idle}); err != nil {
			t.Fatal(err)
		}
	}
	mk("live", clock.Add(time.Hour), clock.Add(time.Hour))
	mk("abs-dead", clock.Add(-time.Hour), clock.Add(time.Hour))
	mk("idle-dead", clock.Add(time.Hour), clock.Add(-time.Hour))

	n, err := s.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("sweep count: want 2, got %d", n)
	}

	// "live" survives; the other two are gone from records AND
	// from the user index (deleteLocked covers both).
	if _, ok := s.records["live"]; !ok {
		t.Error("live record should survive sweep")
	}
	for _, sid := range []string{"abs-dead", "idle-dead"} {
		if _, ok := s.records[sid]; ok {
			t.Errorf("%q should have been swept", sid)
		}
	}
}

func TestMemoryStore_HonorsCancelledContext(t *testing.T) {
	s := NewMemoryStore[cart]()
	// Seed something so each method has data to work against.
	s.Save(context.Background(), Record[cart]{
		SID: "abc", UserID: "u1",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	cases := []struct {
		name string
		call func() error
	}{
		{"Load", func() error { _, err := s.Load(ctx, "abc"); return err }},
		{"Save", func() error {
			_, err := s.Save(ctx, Record[cart]{SID: "xyz", IdleExpiry: time.Now().Add(time.Hour)})
			return err
		}},
		{"Delete", func() error { return s.Delete(ctx, "abc") }},
		{"BumpTTL", func() error { return s.BumpTTL(ctx, "abc", time.Now().Add(time.Hour)) }},
		{"ListByUser", func() error { _, err := s.ListByUser(ctx, "u1"); return err }},
		{"RevokeByUser", func() error { _, err := s.RevokeByUser(ctx, "u1"); return err }},
		{"Scan", func() error { return s.Scan(ctx, func(string) bool { return true }) }},
		{"Sweep", func() error { _, err := s.Sweep(ctx); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Errorf("want context.Canceled, got %v", err)
			}
		})
	}
}

func TestMemoryStore_ImplementsOptionalInterfaces(t *testing.T) {
	var s any = NewMemoryStore[cart]()
	if _, ok := s.(TTLBumper); !ok {
		t.Error("MemoryStore should implement TTLBumper")
	}
	if _, ok := s.(UserIndexer); !ok {
		t.Error("MemoryStore should implement UserIndexer")
	}
	if _, ok := s.(Scanner); !ok {
		t.Error("MemoryStore should implement Scanner")
	}
	if _, ok := s.(Sweeper); !ok {
		t.Error("MemoryStore should implement Sweeper")
	}
}
