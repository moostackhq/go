package session

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"
)

func newCookieJar(t *testing.T, _ string) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return jar
}

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newE2EManager(t *testing.T) (*Manager[cart], *MemoryStore[cart]) {
	t.Helper()
	store := NewMemoryStore[cart]()
	mgr, err := New[cart](Config[cart]{
		Store:          store,
		Token:          Cookie{Name: "sid"},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr, store
}

func TestE2E_AddItemPersistsAcrossRequests(t *testing.T) {
	mgr, store := newE2EManager(t)
	srv := httptest.NewServer(mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/add":
			if err := mgr.Update(r.Context(), func(c *cart) error {
				c.Items = append(c.Items, "milk")
				return nil
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		case "/list":
			p, err := mgr.Get(r.Context())
			if err != nil {
				t.Errorf("get: %v", err)
			}
			for _, item := range p.Items {
				w.Write([]byte(item + "\n"))
			}
		}
	})))
	defer srv.Close()

	client := &http.Client{Jar: newCookieJar(t, srv.URL)}

	// First request: add an item. Response should set the cookie.
	resp, err := client.Get(srv.URL + "/add")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resp.Cookies()) == 0 {
		t.Fatalf("expected Set-Cookie on first write")
	}

	// Second request: the cookie should resolve to the stored cart.
	resp, err = client.Get(srv.URL + "/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if body != "milk\n" {
		t.Errorf("body: want 'milk\\n', got %q", body)
	}

	// Confirm the store actually holds one record.
	var count int
	store.Scan(t.Context(), func(string) bool { count++; return true })
	if count != 1 {
		t.Errorf("store records: want 1, got %d", count)
	}
}

func TestE2E_DestroyClearsCookieAndStore(t *testing.T) {
	mgr, store := newE2EManager(t)
	srv := httptest.NewServer(mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			if err := mgr.Update(r.Context(), func(c *cart) error {
				c.Items = []string{"start"}
				return nil
			}); err != nil {
				t.Error(err)
			}
		case "/logout":
			if err := mgr.Destroy(r.Context()); err != nil {
				t.Error(err)
			}
		}
	})))
	defer srv.Close()

	jar := newCookieJar(t, srv.URL)
	client := &http.Client{Jar: jar}
	if _, err := client.Get(srv.URL + "/login"); err != nil {
		t.Fatal(err)
	}

	// One record present.
	var before int
	store.Scan(t.Context(), func(string) bool { before++; return true })
	if before != 1 {
		t.Fatalf("expected 1 record before logout, got %d", before)
	}

	resp, err := client.Get(srv.URL + "/logout")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Cookie cleared on the response (MaxAge < 0).
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == "sid" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("expected cookie clear on logout, got %v", resp.Cookies())
	}

	// Store record is gone.
	var after int
	store.Scan(t.Context(), func(string) bool { after++; return true })
	if after != 0 {
		t.Errorf("expected 0 records after logout, got %d", after)
	}
}

func TestE2E_RenewRotatesSID(t *testing.T) {
	mgr, store := newE2EManager(t)
	var observedSID string
	srv := httptest.NewServer(mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			mgr.Update(r.Context(), func(c *cart) error { c.Items = []string{"x"}; return nil })
		case "/renew":
			mgr.Renew(r.Context())
		case "/sid":
			sid, _ := mgr.SID(r.Context())
			observedSID = sid
			w.Write([]byte(sid))
		}
	})))
	defer srv.Close()

	client := &http.Client{Jar: newCookieJar(t, srv.URL)}
	client.Get(srv.URL + "/start")
	client.Get(srv.URL + "/sid")
	before := observedSID

	client.Get(srv.URL + "/renew")
	client.Get(srv.URL + "/sid")
	after := observedSID

	if before == "" || after == "" || before == after {
		t.Fatalf("expected SID rotation, before=%q after=%q", before, after)
	}
	if _, err := store.Load(t.Context(), before); !errors.Is(err, ErrNotFound) {
		t.Errorf("old sid should be gone, got %v", err)
	}
	if _, err := store.Load(t.Context(), after); err != nil {
		t.Errorf("new sid should be loadable, got %v", err)
	}
}

func TestE2E_StaleCookieIsIgnored(t *testing.T) {
	mgr, store := newE2EManager(t)
	srv := httptest.NewServer(mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mgr.Update(r.Context(), func(c *cart) error { c.Items = []string{"new"}; return nil })
	})))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "no-such-id"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// A fresh cookie should have been issued.
	var got string
	for _, c := range resp.Cookies() {
		if c.Name == "sid" && c.Value != "" && c.Value != "no-such-id" {
			got = c.Value
		}
	}
	if got == "" {
		t.Fatalf("expected a fresh cookie, got %v", resp.Cookies())
	}
	// And the stale ID was not used as the storage key.
	if _, err := store.Load(t.Context(), "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale sid should not appear in store, got %v", err)
	}
}

func TestE2E_CookieRefreshesOnEachTouchedRequest(t *testing.T) {
	store := NewMemoryStore[cart]()
	clock := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return clock }
	mgr, err := New[cart](Config[cart]{
		Store:            store,
		Token:            Cookie{Name: "sid"},
		AbsoluteExpiry:   24 * time.Hour,
		IdleExpiry:       30 * time.Minute,
		IdleBumpInterval: 5 * time.Minute,
		Now:              func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			mgr.Update(r.Context(), func(c *cart) error { c.Items = []string{"x"}; return nil })
			return
		}
		mgr.Get(r.Context()) // read-only path
	}))

	// First request: mint the session, capture cookie.
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, httptest.NewRequest("GET", "/start", nil))
	c1 := pickCookie(t, rr1.Result(), "sid")
	sid := c1.Value

	// Advance clock past the debounce window; make a read-only
	// request. Cookie must be refreshed with a later expiry.
	clock = clock.Add(10 * time.Minute)
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	handler.ServeHTTP(rr2, req2)
	c2 := pickCookie(t, rr2.Result(), "sid")
	if c2.Value != sid {
		t.Errorf("refresh should preserve SID: before=%q after=%q", sid, c2.Value)
	}
	if !c2.Expires.After(c1.Expires) {
		t.Errorf("refreshed cookie should expire later: before=%v after=%v", c1.Expires, c2.Expires)
	}
}

func pickCookie(t *testing.T, r *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q cookie in response (cookies=%v)", name, r.Cookies())
	return nil
}

func TestE2E_HandlerNoWriteStillEmitsCookie(t *testing.T) {
	mgr, _ := newE2EManager(t)
	srv := httptest.NewServer(mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mgr.Update(r.Context(), func(c *cart) error { c.Items = []string{"a"}; return nil })
		// Deliberately do not write anything.
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
	if len(resp.Cookies()) == 0 {
		t.Errorf("expected Set-Cookie even with empty body")
	}
}

// countingRecorder is an httptest.ResponseRecorder that counts how
// many times WriteHeader landed on it. Used to verify commit-failure
// paths don't double-write.
type countingRecorder struct {
	*httptest.ResponseRecorder
	writeHeaderCalls int
}

func (c *countingRecorder) WriteHeader(status int) {
	c.writeHeaderCalls++
	c.ResponseRecorder.WriteHeader(status)
}

func TestE2E_CommitFailureDoesNotDoubleWriteHeader(t *testing.T) {
	// Set up a manager whose Destroy commit will fail (Store.Delete
	// errors). The middleware's runCommit calls http.Error → one
	// WriteHeader(500). The tail path then sees !headerWritten ...
	// wait, runCommit sets headerWritten=true on http.Error. The
	// tail path's cw.WriteHeader(200) must NOT issue a second
	// WriteHeader on the underlying.
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

	rr := &countingRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: seed.SID})

	mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = mgr.Destroy(r.Context())
		// Handler writes nothing — forces Middleware's tail path to fire.
	})).ServeHTTP(rr, req)

	if rr.writeHeaderCalls != 1 {
		t.Errorf("WriteHeader should fire exactly once even when commit fails; got %d calls (status=%d)",
			rr.writeHeaderCalls, rr.Code)
	}
}

// deleteFailingStore is defined in manager_test.go.

// hijackableRecorder is an httptest.ResponseRecorder that also
// implements http.Hijacker. Its WriteHeader records whether the
// stdlib middleware tail path issued a stray write after the
// connection was "hijacked", which is the bug we're guarding against.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked         bool
	writeHeaderAfter bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func (h *hijackableRecorder) WriteHeader(status int) {
	if h.hijacked {
		h.writeHeaderAfter = true
	}
	h.ResponseRecorder.WriteHeader(status)
}

func TestE2E_HijackPreventsStrayWriteHeaderFromTailPath(t *testing.T) {
	mgr, _ := newE2EManager(t)
	fake := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest("GET", "/", nil)

	mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("committingWriter should expose http.Hijacker")
		}
		if _, _, err := hj.Hijack(); err != nil {
			t.Fatalf("hijack: %v", err)
		}
		// Handler returns here. Middleware's tail path would normally
		// call WriteHeader(200) — must not on a hijacked writer.
	})).ServeHTTP(fake, req)

	if !fake.hijacked {
		t.Error("expected underlying Hijack to be invoked")
	}
	if fake.writeHeaderAfter {
		t.Error("middleware called WriteHeader after Hijack — stray write to hijacked connection")
	}
}
