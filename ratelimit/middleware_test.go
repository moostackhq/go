package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func doReq(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_LimitsAndHeaders(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: 2}, WithClock(clk.now))
	h := Middleware(lim)(okHandler())

	for i := 0; i < 2; i++ {
		rec := doReq(h, "10.0.0.1:1234")
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d code = %d, want 200", i+1, rec.Code)
		}
		if got := rec.Header().Get("RateLimit-Limit"); got != "2" {
			t.Errorf("req %d RateLimit-Limit = %q, want 2", i+1, got)
		}
	}

	rec := doReq(h, "10.0.0.1:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd code = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After")
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("RateLimit-Remaining = %q, want 0", got)
	}
}

func TestMiddleware_PerIPIsolation(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second, Burst: 1}, WithClock(clk.now))
	h := Middleware(lim)(okHandler())

	if c := doReq(h, "1.1.1.1:1").Code; c != http.StatusOK {
		t.Fatalf("A first = %d, want 200", c)
	}
	if c := doReq(h, "1.1.1.1:1").Code; c != http.StatusTooManyRequests {
		t.Fatalf("A second = %d, want 429", c)
	}
	if c := doReq(h, "2.2.2.2:1").Code; c != http.StatusOK {
		t.Fatalf("B first = %d, want 200 (IPs must be independent)", c)
	}
}

func TestMiddleware_FailOpenAndClosed(t *testing.T) {
	lim, _ := New(errStore{}, Limit{Rate: 1, Period: time.Second})

	open := Middleware(lim)(okHandler())
	if c := doReq(open, "1.2.3.4:1").Code; c != http.StatusOK {
		t.Errorf("fail-open on store error = %d, want 200", c)
	}

	closed := Middleware(lim, WithFailClosed())(okHandler())
	if c := doReq(closed, "1.2.3.4:1").Code; c != http.StatusServiceUnavailable {
		t.Errorf("fail-closed on store error = %d, want 503", c)
	}
}

func TestMiddleware_EmptyKeyAppliesPolicy(t *testing.T) {
	lim, _ := New(NewMemoryStore(), Limit{Rate: 1, Period: time.Second})
	emptyKey := WithKeyFunc(func(*http.Request) string { return "" })

	open := Middleware(lim, emptyKey)(okHandler())
	if c := doReq(open, "1.2.3.4:1").Code; c != http.StatusOK {
		t.Errorf("empty key, fail-open = %d, want 200", c)
	}
	closed := Middleware(lim, emptyKey, WithFailClosed())(okHandler())
	if c := doReq(closed, "1.2.3.4:1").Code; c != http.StatusServiceUnavailable {
		t.Errorf("empty key, fail-closed = %d, want 503", c)
	}
}

func TestMiddleware_HeaderValues(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	lim, _ := New(NewMemoryStore(), PerMinute(3), WithClock(clk.now)) // emission 20s, dvt 60s
	h := Middleware(lim)(okHandler())

	for i := 0; i < 3; i++ {
		if c := doReq(h, "9.9.9.9:1").Code; c != http.StatusOK {
			t.Fatalf("req %d code = %d, want 200", i+1, c)
		}
	}
	rec := doReq(h, "9.9.9.9:1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th code = %d, want 429", rec.Code)
	}
	for _, c := range []struct{ name, want string }{
		{"RateLimit-Limit", "3"},
		{"RateLimit-Remaining", "0"},
		{"RateLimit-Reset", "60"}, // bucket fully clears in 60s
		{"Retry-After", "20"},     // next token in 20s
	} {
		if got := rec.Header().Get(c.name); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestKeyByIP(t *testing.T) {
	cases := []struct{ remote, want string }{
		{"203.0.113.5:443", "203.0.113.5"},
		{"[2001:db8::1]:443", "2001:db8::1"}, // IPv6 host stripped of brackets
		{"noport", "noport"},                 // unparseable → used as-is
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if got := KeyByIP(r); got != c.want {
			t.Errorf("KeyByIP(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}

// errStore fails every operation, to exercise the degraded path.
type errStore struct{}

func (errStore) Get(context.Context, string) (int64, bool, error) {
	return 0, false, errors.New("boom")
}
func (errStore) SetIfAbsent(context.Context, string, int64, time.Duration) (bool, error) {
	return false, errors.New("boom")
}
func (errStore) CompareAndSwap(context.Context, string, int64, int64, time.Duration) (bool, error) {
	return false, errors.New("boom")
}
