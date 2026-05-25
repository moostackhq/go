package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestManager(t *testing.T) (*Manager[cart], *MemoryStore[cart]) {
	t.Helper()
	store := NewMemoryStore[cart]()
	mgr, err := New[cart](Config[cart]{
		Store:          store,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr, store
}

func TestNew_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config[cart]
		want string
	}{
		{"no store", Config[cart]{Token: Cookie{}, AbsoluteExpiry: time.Hour, IdleExpiry: time.Minute}, "Store"},
		{"no token", Config[cart]{Store: NewMemoryStore[cart](), AbsoluteExpiry: time.Hour, IdleExpiry: time.Minute}, "Token"},
		{"no abs", Config[cart]{Store: NewMemoryStore[cart](), Token: Cookie{}, IdleExpiry: time.Minute}, "AbsoluteExpiry"},
		{"no idle", Config[cart]{Store: NewMemoryStore[cart](), Token: Cookie{}, AbsoluteExpiry: time.Hour}, "IdleExpiry"},
		{"idle gt abs", Config[cart]{Store: NewMemoryStore[cart](), Token: Cookie{}, AbsoluteExpiry: time.Minute, IdleExpiry: time.Hour}, "IdleExpiry"},
		{"bump gt idle", Config[cart]{Store: NewMemoryStore[cart](), Token: Cookie{}, AbsoluteExpiry: time.Hour, IdleExpiry: time.Minute, IdleBumpInterval: time.Hour}, "IdleBumpInterval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New[cart](tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestNew_ReportsAllConfigErrorsAtOnce(t *testing.T) {
	// Empty config: every required field is bad. New should surface
	// all four problems in one error, not just the first.
	_, err := New[cart](Config[cart]{})
	if err == nil {
		t.Fatal("expected error from empty config")
	}
	msg := err.Error()
	for _, want := range []string{"Store", "Token", "AbsoluteExpiry", "IdleExpiry"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q in joined output, got %q", want, msg)
		}
	}
}

func TestManager_GetWithoutContextReturnsErrNoSession(t *testing.T) {
	mgr, _ := newTestManager(t)
	if _, err := mgr.Get(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestManager_NoSessionErrorIdentifiesMethod(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, err := mgr.Get(ctx); return err }},
		{"Save", func() error { return mgr.Save(ctx) }},
		{"Update", func() error { return mgr.Update(ctx, func(*cart) error { return nil }) }},
		{"Destroy", func() error { return mgr.Destroy(ctx) }},
		{"Renew", func() error { return mgr.Renew(ctx) }},
		{"SID", func() error { _, err := mgr.SID(ctx); return err }},
		{"UserID", func() error { _, err := mgr.UserID(ctx); return err }},
		{"Promote", func() error { return mgr.Promote(ctx, "x") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, ErrNoSession) {
				t.Fatalf("want ErrNoSession, got %v", err)
			}
			if !strings.Contains(err.Error(), "session."+tc.name+":") {
				t.Errorf("error should be wrapped with method name %q, got %q", tc.name, err.Error())
			}
		})
	}
}

func TestManager_LazyLoadAndUpdate(t *testing.T) {
	mgr, _ := newTestManager(t)
	st := &state[cart]{}
	ctx := context.WithValue(context.Background(), mgr.ctxKey, st)

	// First Get on a fresh request: no SID, no store hit.
	p, err := mgr.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 0 {
		t.Fatalf("new session payload should be zero, got %+v", p)
	}
	if st.sid != "" {
		t.Errorf("expected sid empty before any commit, got %q", st.sid)
	}

	// Update writes through; SID is generated and dirty is set.
	err = mgr.Update(ctx, func(p *cart) error {
		p.Items = append(p.Items, "milk")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.sid == "" {
		t.Errorf("Update should have generated a SID")
	}
	if !st.dirty {
		t.Errorf("Update should mark state dirty")
	}
	if st.sid == st.origSID {
		t.Errorf("Update should have moved sid past origSID for cookie emission")
	}
}

// flakyStore is a Store that forces a CAS conflict on Save until
// allowAfter attempts have passed, then delegates to an inner
// MemoryStore. Used to verify Update's retry loop.
type flakyStore struct {
	inner      *MemoryStore[cart]
	conflicts  int32
	allowAfter int32
}

func (s *flakyStore) Load(ctx context.Context, sid string) (Record[cart], error) {
	return s.inner.Load(ctx, sid)
}
func (s *flakyStore) Save(ctx context.Context, rec Record[cart]) (Record[cart], error) {
	n := atomic.AddInt32(&s.conflicts, 1)
	if n <= s.allowAfter {
		return Record[cart]{}, fmt.Errorf("forced: %w", ErrVersionConflict)
	}
	return s.inner.Save(ctx, rec)
}
func (s *flakyStore) Delete(ctx context.Context, sid string) error { return s.inner.Delete(ctx, sid) }

func TestManager_UpdateRetriesOnCASConflict(t *testing.T) {
	inner := NewMemoryStore[cart]()
	// Seed a record so Load returns a real value during retry.
	seed, _ := inner.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
		Payload:        cart{Items: []string{"seed"}},
	})

	flaky := &flakyStore{inner: inner, allowAfter: 2}
	mgr, err := New[cart](Config[cart]{
		Store:          flaky,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Minute,
		MaxRetries:     3,
	})
	if err != nil {
		t.Fatal(err)
	}

	st := &state[cart]{sid: seed.SID}
	ctx := context.WithValue(context.Background(), mgr.ctxKey, st)
	calls := 0
	if err := mgr.Update(ctx, func(p *cart) error {
		calls++
		p.Items = append(p.Items, "added")
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 3 {
		t.Errorf("closure call count: want 3, got %d", calls)
	}
	got, _ := inner.Load(ctx, "abc")
	want := []string{"seed", "added"}
	if len(got.Payload.Items) != len(want) || got.Payload.Items[0] != want[0] || got.Payload.Items[1] != want[1] {
		t.Errorf("payload: want %v, got %v", want, got.Payload.Items)
	}
}

func TestManager_UpdateGivesUpAfterMaxRetries(t *testing.T) {
	inner := NewMemoryStore[cart]()
	inner.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})
	flaky := &flakyStore{inner: inner, allowAfter: 1000}
	mgr, err := New[cart](Config[cart]{
		Store:          flaky,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Minute,
		MaxRetries:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &state[cart]{sid: "abc"}
	ctx := context.WithValue(context.Background(), mgr.ctxKey, st)
	err = mgr.Update(ctx, func(p *cart) error { return nil })
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict after exhausting retries, got %v", err)
	}
}

func TestManager_Promote_SetsUserIDAndRotatesSID(t *testing.T) {
	mgr, store := newTestManager(t)
	// Seed an existing session.
	seed, _ := store.Save(context.Background(), Record[cart]{
		SID:            "old-sid",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
		Payload:        cart{Items: []string{"keep"}},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: seed.SID})

	mgr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Promote(r.Context(), "user-42"); err != nil {
			t.Errorf("promote: %v", err)
		}
	})).ServeHTTP(rr, req)

	// Old SID is gone.
	if _, err := store.Load(context.Background(), seed.SID); !errors.Is(err, ErrNotFound) {
		t.Errorf("old SID should be deleted, got %v", err)
	}

	// New SID exists and carries the new UserID + preserved payload.
	var newSID string
	for _, c := range rr.Result().Cookies() {
		if c.Name == "sid" {
			newSID = c.Value
		}
	}
	if newSID == "" || newSID == seed.SID {
		t.Fatalf("expected fresh SID cookie, got %q (old=%q)", newSID, seed.SID)
	}
	got, err := store.Load(context.Background(), newSID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != "user-42" {
		t.Errorf("UserID: want user-42, got %q", got.UserID)
	}
	if len(got.Payload.Items) != 1 || got.Payload.Items[0] != "keep" {
		t.Errorf("payload should survive Promote: got %+v", got.Payload)
	}
}

func TestManager_UserID_NoSession(t *testing.T) {
	mgr, _ := newTestManager(t)
	if _, err := mgr.UserID(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestManager_UserID_ReadsLoadedRecord(t *testing.T) {
	mgr, store := newTestManager(t)
	store.Save(context.Background(), Record[cart]{
		SID:            "abc",
		UserID:         "alice",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})
	st := &state[cart]{sid: "abc", origSID: "abc"}
	ctx := context.WithValue(context.Background(), mgr.ctxKey, st)
	uid, err := mgr.UserID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "alice" {
		t.Errorf("UserID: want alice, got %q", uid)
	}
}

func TestManager_UserID_ReflectsPromoteInSameRequest(t *testing.T) {
	mgr, store := newTestManager(t)
	seed, _ := store.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: seed.SID})

	var observed string
	mgr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Promote(r.Context(), "user-42"); err != nil {
			t.Fatal(err)
		}
		uid, err := mgr.UserID(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		observed = uid
	})).ServeHTTP(rr, req)
	if observed != "user-42" {
		t.Errorf("UserID after Promote in same request: want user-42, got %q", observed)
	}
}

func TestManager_ListForUser(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	for _, sid := range []string{"a", "b"} {
		store.Save(ctx, Record[cart]{
			SID:            sid,
			UserID:         "u1",
			AbsoluteExpiry: time.Now().Add(time.Hour),
			IdleExpiry:     time.Now().Add(time.Hour),
		})
	}
	got, err := mgr.ListForUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 sessions for u1, got %d", len(got))
	}
}

func TestManager_RevokeAllForUser(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	for _, sid := range []string{"a", "b", "c"} {
		store.Save(ctx, Record[cart]{
			SID:            sid,
			UserID:         "u1",
			AbsoluteExpiry: time.Now().Add(time.Hour),
			IdleExpiry:     time.Now().Add(time.Hour),
		})
	}
	n, err := mgr.RevokeAllForUser(ctx, "u1", "b")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("revoked: want 2, got %d", n)
	}
	if _, err := store.Load(ctx, "b"); err != nil {
		t.Errorf("excepted session should survive: %v", err)
	}
}

// deleteFailingStore wraps a MemoryStore but always errors on Delete.
type deleteFailingStore struct{ inner *MemoryStore[cart] }

func (s *deleteFailingStore) Load(ctx context.Context, sid string) (Record[cart], error) {
	return s.inner.Load(ctx, sid)
}
func (s *deleteFailingStore) Save(ctx context.Context, rec Record[cart]) (Record[cart], error) {
	return s.inner.Save(ctx, rec)
}
func (s *deleteFailingStore) Delete(context.Context, string) error {
	return errors.New("disk gremlins")
}

func TestManager_Destroy_ClearsCookieEvenWhenDeleteFails(t *testing.T) {
	inner := NewMemoryStore[cart]()
	seed, _ := inner.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})
	mgr, err := New[cart](Config[cart]{
		Store:          &deleteFailingStore{inner: inner},
		Token:          Cookie{Name: "sid"},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: seed.SID})
	mgr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = mgr.Destroy(r.Context())
	})).ServeHTTP(rr, req)

	// The commit will fail (delete errors), so the middleware
	// surfaces a 500. The Set-Cookie clearing the session must
	// still ship with that response.
	var cleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "sid" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("cookie should be cleared even when Delete fails (status=%d, cookies=%v)",
			rr.Code, rr.Result().Cookies())
	}
}

// noUserIndexStore is a Store with no UserIndexer implementation, used
// to verify Manager surfaces ErrCapabilityMissing.
type noUserIndexStore struct{}

func (noUserIndexStore) Load(context.Context, string) (Record[cart], error) {
	return Record[cart]{}, ErrNotFound
}
func (noUserIndexStore) Save(context.Context, Record[cart]) (Record[cart], error) {
	return Record[cart]{}, nil
}
func (noUserIndexStore) Delete(context.Context, string) error { return nil }

func TestManager_IdentityAPIRequiresUserIndexer(t *testing.T) {
	mgr, err := New[cart](Config[cart]{
		Store:          noUserIndexStore{},
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ListForUser(context.Background(), "u"); !errors.Is(err, ErrCapabilityMissing) {
		t.Errorf("ListForUser: want ErrCapabilityMissing, got %v", err)
	}
	if _, err := mgr.RevokeAllForUser(context.Background(), "u"); !errors.Is(err, ErrCapabilityMissing) {
		t.Errorf("RevokeAllForUser: want ErrCapabilityMissing, got %v", err)
	}
}

func TestManager_TTLBumpDebounce(t *testing.T) {
	store := NewMemoryStore[cart]()
	clock := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return clock }

	mgr, err := New[cart](Config[cart]{
		Store:            store,
		Token:            Cookie{Name: "sid"},
		AbsoluteExpiry:   time.Hour,
		IdleExpiry:       30 * time.Minute,
		IdleBumpInterval: 5 * time.Minute,
		Now:              func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed an existing session.
	seed, _ := store.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: clock.Add(time.Hour),
		IdleExpiry:     clock.Add(30 * time.Minute),
	})

	readOnce := func() time.Time {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "sid", Value: seed.SID})
		mgr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mgr.Get(r.Context()) // read-only
		})).ServeHTTP(rr, req)
		got, _ := store.Load(context.Background(), seed.SID)
		return got.IdleExpiry
	}

	idleBefore := readOnce()

	// Tick forward by less than the debounce interval — no bump.
	clock = clock.Add(2 * time.Minute)
	if got := readOnce(); !got.Equal(idleBefore) {
		t.Errorf("bump fired inside debounce window: before=%v after=%v", idleBefore, got)
	}

	// Tick past the debounce threshold — bump fires.
	clock = clock.Add(10 * time.Minute) // total 12 min elapsed, > 5
	got := readOnce()
	wantMin := clock.Add(30 * time.Minute)
	if !got.Equal(wantMin) {
		t.Errorf("bump did not fire: idle_expiry=%v want=%v", got, wantMin)
	}
}

func TestManager_MutatingMethodsAfterDestroyReturnErrSessionDestroyed(t *testing.T) {
	mgr, store := newTestManager(t)
	seed, _ := store.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})

	run := func(t *testing.T, fn func(ctx context.Context, mgr *Manager[cart]) error) {
		t.Helper()
		st := &state[cart]{sid: seed.SID, origSID: seed.SID}
		ctx := context.WithValue(context.Background(), mgr.ctxKey, st)
		if err := mgr.Destroy(ctx); err != nil {
			t.Fatal(err)
		}
		if err := fn(ctx, mgr); !errors.Is(err, ErrSessionDestroyed) {
			t.Errorf("want ErrSessionDestroyed, got %v", err)
		}
	}

	t.Run("Save", func(t *testing.T) {
		run(t, func(ctx context.Context, m *Manager[cart]) error { return m.Save(ctx) })
	})
	t.Run("Update", func(t *testing.T) {
		run(t, func(ctx context.Context, m *Manager[cart]) error {
			return m.Update(ctx, func(*cart) error { return nil })
		})
	})
	t.Run("Renew", func(t *testing.T) {
		run(t, func(ctx context.Context, m *Manager[cart]) error { return m.Renew(ctx) })
	})
	t.Run("Promote", func(t *testing.T) {
		run(t, func(ctx context.Context, m *Manager[cart]) error { return m.Promote(ctx, "x") })
	})
}

type bag struct {
	Items map[string]int
}

func TestManager_Update_LeaksReferenceMutationsWithoutCloner(t *testing.T) {
	// Documents the default (shallow-copy) behaviour: a closure
	// that mutates a map and then returns an error leaves the
	// mutation visible to a subsequent Get. This is the contract
	// the Cloner field exists to flip.
	store := NewMemoryStore[bag]()
	seed, _ := store.Save(context.Background(), Record[bag]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
		Payload:        bag{Items: map[string]int{"a": 1}},
	})
	mgr, err := New[bag](Config[bag]{
		Store:          store,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &state[bag]{sid: seed.SID, origSID: seed.SID}
	ctx := context.WithValue(context.Background(), mgr.ctxKey, st)

	_, _ = mgr.Get(ctx) // populate st.payload from store

	_ = mgr.Update(ctx, func(b *bag) error {
		b.Items["leaked"] = 2
		return errors.New("intentional")
	})

	p, _ := mgr.Get(ctx)
	if _, ok := p.Items["leaked"]; !ok {
		t.Fatalf("baseline check: expected the leak (this test documents it). Got map=%v", p.Items)
	}
}

func TestManager_Update_ClonerIsolatesReferenceMutations(t *testing.T) {
	store := NewMemoryStore[bag]()
	seed, _ := store.Save(context.Background(), Record[bag]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
		Payload:        bag{Items: map[string]int{"a": 1}},
	})
	mgr, err := New[bag](Config[bag]{
		Store:          store,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
		Cloner: func(b bag) bag {
			out := bag{Items: make(map[string]int, len(b.Items))}
			for k, v := range b.Items {
				out.Items[k] = v
			}
			return out
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &state[bag]{sid: seed.SID, origSID: seed.SID}
	ctx := context.WithValue(context.Background(), mgr.ctxKey, st)

	_, _ = mgr.Get(ctx)

	_ = mgr.Update(ctx, func(b *bag) error {
		b.Items["should-not-leak"] = 2
		return errors.New("intentional")
	})

	p, _ := mgr.Get(ctx)
	if _, ok := p.Items["should-not-leak"]; ok {
		t.Errorf("Cloner should have isolated the map mutation; got map=%v", p.Items)
	}
	if v, ok := p.Items["a"]; !ok || v != 1 {
		t.Errorf("original map content lost: %v", p.Items)
	}
}

// memoryNoUserIndex wraps a MemoryStore but does NOT expose its
// UserIndexer methods, used to verify the Promote contract against a
// store that can't index users.
type memoryNoUserIndex struct{ inner *MemoryStore[cart] }

func (s *memoryNoUserIndex) Load(ctx context.Context, sid string) (Record[cart], error) {
	return s.inner.Load(ctx, sid)
}
func (s *memoryNoUserIndex) Save(ctx context.Context, rec Record[cart]) (Record[cart], error) {
	return s.inner.Save(ctx, rec)
}
func (s *memoryNoUserIndex) Delete(ctx context.Context, sid string) error {
	return s.inner.Delete(ctx, sid)
}

func TestManager_PromoteSucceedsWithoutUserIndexer(t *testing.T) {
	// Promote sets Record.UserID and rotates the SID — neither
	// requires UserIndexer. The capability gap only matters at
	// query time (ListForUser / RevokeAllForUser). This test
	// nails down that split so a future refactor doesn't tighten
	// Promote's preconditions silently.
	inner := NewMemoryStore[cart]()
	seed, _ := inner.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})
	mgr, err := New[cart](Config[cart]{
		Store:          &memoryNoUserIndex{inner: inner},
		Token:          Cookie{Name: "sid"},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: seed.SID})
	mgr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Promote(r.Context(), "alice"); err != nil {
			t.Errorf("Promote should succeed without UserIndexer, got %v", err)
		}
	})).ServeHTTP(rr, req)

	// ListForUser is the operation that genuinely needs UserIndexer.
	if _, err := mgr.ListForUser(context.Background(), "alice"); !errors.Is(err, ErrCapabilityMissing) {
		t.Errorf("ListForUser should fail with ErrCapabilityMissing on a non-UserIndexer store, got %v", err)
	}
}

// ctxAwareFlakyStore forces a CAS conflict on the first Save and
// honours context cancellation on subsequent operations. Used to
// verify Update aborts cleanly when the request is cancelled
// between retries.
type ctxAwareFlakyStore struct {
	inner *MemoryStore[cart]
	saves atomic.Int32
}

func (s *ctxAwareFlakyStore) Load(ctx context.Context, sid string) (Record[cart], error) {
	if err := ctx.Err(); err != nil {
		return Record[cart]{}, err
	}
	return s.inner.Load(ctx, sid)
}
func (s *ctxAwareFlakyStore) Save(ctx context.Context, rec Record[cart]) (Record[cart], error) {
	if s.saves.Add(1) == 1 {
		return Record[cart]{}, fmt.Errorf("forced: %w", ErrVersionConflict)
	}
	if err := ctx.Err(); err != nil {
		return Record[cart]{}, err
	}
	return s.inner.Save(ctx, rec)
}
func (s *ctxAwareFlakyStore) Delete(ctx context.Context, sid string) error {
	return s.inner.Delete(ctx, sid)
}

func TestManager_UpdateRespectsCancelledContextMidRetry(t *testing.T) {
	inner := NewMemoryStore[cart]()
	seed, _ := inner.Save(context.Background(), Record[cart]{
		SID:            "abc",
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(time.Hour),
	})
	flaky := &ctxAwareFlakyStore{inner: inner}
	mgr, err := New[cart](Config[cart]{
		Store:          flaky,
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Minute,
		MaxRetries:     5,
	})
	if err != nil {
		t.Fatal(err)
	}

	st := &state[cart]{sid: seed.SID, origSID: seed.SID}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, mgr.ctxKey, st)

	calls := 0
	err = mgr.Update(ctx, func(c *cart) error {
		calls++
		if calls == 1 {
			// First save will force ErrVersionConflict. Cancel
			// the context now so the retry's Load sees it.
			cancel()
		}
		c.Items = append(c.Items, "x")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Update should bubble up context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("closure should not re-run after cancellation: got %d calls", calls)
	}
}
