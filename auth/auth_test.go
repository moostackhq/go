package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/moostackhq/go/auth"
)

// --- helpers ---

// stubAuth returns a fixed (Identity, error) on every call. Use to
// drive chain-order tests without spinning up real backends.
type stubAuth struct {
	id    auth.Identity
	err   error
	calls *int
}

func (s *stubAuth) Authenticate(r *http.Request) (auth.Identity, error) {
	if s.calls != nil {
		*s.calls++
	}
	return s.id, s.err
}

// --- Chain ---

func TestChain_FirstSuccessWins(t *testing.T) {
	first := &stubAuth{id: auth.Identity{Subject: "first"}}
	second := &stubAuth{id: auth.Identity{Subject: "second"}}

	chain := auth.Chain(first, second)
	id, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Subject != "first" {
		t.Errorf("Subject = %q, want first", id.Subject)
	}
}

func TestChain_FallthroughOnUnauthenticated(t *testing.T) {
	first := &stubAuth{err: auth.ErrUnauthenticated}
	second := &stubAuth{id: auth.Identity{Subject: "second"}}

	chain := auth.Chain(first, second)
	id, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.Subject != "second" {
		t.Errorf("Subject = %q, want second", id.Subject)
	}
}

func TestChain_NonSentinelErrorShortCircuits(t *testing.T) {
	// A store outage on the first authenticator must not silently
	// fall through to a less-trusted authenticator.
	outage := errors.New("redis: connection refused")
	secondCalls := 0
	first := &stubAuth{err: outage}
	second := &stubAuth{id: auth.Identity{Subject: "second"}, calls: &secondCalls}

	chain := auth.Chain(first, second)
	_, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, outage) {
		t.Errorf("err = %v, want outage", err)
	}
	if secondCalls != 0 {
		t.Errorf("second was called %d times, want 0", secondCalls)
	}
}

func TestChain_EmptyReturnsUnauthenticated(t *testing.T) {
	_, err := auth.Chain().Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestChain_AllFallthroughReturnsUnauthenticated(t *testing.T) {
	first := &stubAuth{err: auth.ErrUnauthenticated}
	second := &stubAuth{err: auth.ErrUnauthenticated}

	chain := auth.Chain(first, second)
	_, err := chain.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestChain_PanicsOnNilEntry pins the fail-loud-at-construction
// contract: a nil Authenticator must panic at Chain time, not later
// inside a request handler. The panic message must name the offending
// index so configuration bugs are diagnosable from the stack alone.
func TestChain_PanicsOnNilEntry(t *testing.T) {
	cases := []struct {
		name      string
		args      []auth.Authenticator
		wantIndex string
	}{
		{
			name:      "single nil",
			args:      []auth.Authenticator{nil},
			wantIndex: "index 0",
		},
		{
			name:      "nil first",
			args:      []auth.Authenticator{nil, &stubAuth{}},
			wantIndex: "index 0",
		},
		{
			name:      "nil middle",
			args:      []auth.Authenticator{&stubAuth{}, nil, &stubAuth{}},
			wantIndex: "index 1",
		},
		{
			name:      "nil last",
			args:      []auth.Authenticator{&stubAuth{}, &stubAuth{}, nil},
			wantIndex: "index 2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				v := recover()
				if v == nil {
					t.Fatal("expected panic on nil Authenticator entry")
				}
				msg, _ := v.(string)
				if !strings.Contains(msg, "nil Authenticator") {
					t.Errorf("panic = %q, want message mentioning 'nil Authenticator'", msg)
				}
				if !strings.Contains(msg, tc.wantIndex) {
					t.Errorf("panic = %q, want it to name %q so the offending position is obvious", msg, tc.wantIndex)
				}
			}()
			_ = auth.Chain(tc.args...)
		})
	}
}

func TestAuthenticatorFunc_Adapter(t *testing.T) {
	want := auth.Identity{Subject: "u"}
	a := auth.AuthenticatorFunc(func(r *http.Request) (auth.Identity, error) {
		return want, nil
	})
	id, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil || id.Subject != "u" {
		t.Errorf("got (%+v, %v), want (%+v, nil)", id, err, want)
	}
}

// --- Identifiable ---

// TestIdentity_SatisfiesIdentifiable pins the contract that *Identity
// itself implements Identifiable, so session.Manager[auth.Identity]
// works with password.New without a wrapping struct.
func TestIdentity_SatisfiesIdentifiable(t *testing.T) {
	id := &auth.Identity{Subject: "u-1", Email: "u@example.com"}
	var _ auth.Identifiable = id

	got := id.AuthIdentity()
	if got != id {
		t.Errorf("AuthIdentity() returned %p, want self (%p) — must return a writable pointer", got, id)
	}

	// Writes through the returned pointer must mutate the original.
	got.Email = "changed@example.com"
	if id.Email != "changed@example.com" {
		t.Error("write through AuthIdentity() did not mutate the underlying Identity")
	}
}

// TestIdentifiable_EmbeddingPattern is the primary user-facing form:
// AppSession embeds auth.Identity by value, *AppSession picks up the
// promoted AuthIdentity method, and writes through the promoted
// method mutate the embedded field — not a copy. No method on
// AppSession itself.
func TestIdentifiable_EmbeddingPattern(t *testing.T) {
	type AppSession struct {
		auth.Identity
		Locale string
	}

	s := &AppSession{Locale: "en"}
	var _ auth.Identifiable = s // compile-time check via promotion

	// Mutate via the promoted method — must hit the embedded field
	// inside this particular AppSession, not a returned copy.
	s.AuthIdentity().Subject = "u-1"
	if s.Subject != "u-1" {
		t.Error("write through promoted AuthIdentity did not mutate the embedded Identity")
	}
	if s.Locale != "en" {
		t.Errorf("unrelated field clobbered: Locale = %q", s.Locale)
	}
}

// TestIdentifiable_NamedFieldPattern is the fallback form for apps
// that need a custom layout or a specific JSON shape. The app writes
// one method on *AppSession returning a writable pointer to the
// named field.
func TestIdentifiable_NamedFieldPattern(t *testing.T) {
	type AppSession struct {
		Identity auth.Identity
		Locale   string
	}
	getIdentity := func(s *AppSession) *auth.Identity { return &s.Identity }

	s := &AppSession{Locale: "en"}
	getIdentity(s).Subject = "u-1"
	if s.Identity.Subject != "u-1" {
		t.Error("write through accessor did not mutate the Identity field")
	}
	if s.Locale != "en" {
		t.Errorf("unrelated field clobbered: Locale = %q", s.Locale)
	}
}

// --- Required / Optional middleware ---

func TestRequired_NilAuthenticatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil Authenticator")
		}
	}()
	_ = auth.Required(nil)
}

func TestOptional_NilAuthenticatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil Authenticator")
		}
	}()
	_ = auth.Optional(nil)
}

func TestRequired_401OnMiss(t *testing.T) {
	a := &stubAuth{err: auth.ErrUnauthenticated}
	called := false
	h := auth.Required(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if called {
		t.Error("next handler ran despite 401")
	}
}

func TestRequired_StashesIdentity(t *testing.T) {
	want := auth.Identity{Subject: "u", Email: "u@example.com", Provider: "test"}
	a := &stubAuth{id: want}

	var got auth.Identity
	var ok bool
	h := auth.Required(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = auth.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !ok {
		t.Fatal("FromContext returned ok=false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
}

func TestRequired_500OnStoreError(t *testing.T) {
	a := &stubAuth{err: errors.New("redis: down")}
	called := false
	h := auth.Required(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
	if called {
		t.Error("next handler ran despite store error")
	}
}

func TestOptional_PassesThroughOnMiss(t *testing.T) {
	a := &stubAuth{err: auth.ErrUnauthenticated}
	called := false
	var fromCtx auth.Identity
	var ok bool
	h := auth.Optional(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		fromCtx, ok = auth.FromContext(r.Context())
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Error("next handler did not run")
	}
	if ok {
		t.Errorf("FromContext ok=true, want false; got %+v", fromCtx)
	}
}

func TestOptional_StashesWhenAuthenticated(t *testing.T) {
	want := auth.Identity{Subject: "u"}
	a := &stubAuth{id: want}

	var got auth.Identity
	var ok bool
	h := auth.Optional(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = auth.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !ok {
		t.Fatal("FromContext ok=false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
}

func TestOptional_500OnStoreError(t *testing.T) {
	a := &stubAuth{err: errors.New("redis: down")}
	called := false
	h := auth.Optional(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
	if called {
		t.Error("next handler ran despite store error")
	}
}
