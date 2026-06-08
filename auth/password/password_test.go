package password_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moostackhq/go/auth"
	"github.com/moostackhq/go/auth/password"
	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/session"
)

// --- in-memory user store ---

type memStore struct {
	mu    sync.Mutex
	users map[string]password.User // key: lowercased email
}

func newMemStore() *memStore { return &memStore{users: map[string]password.User{}} }

func (s *memStore) LookupByEmail(_ context.Context, email string) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[strings.ToLower(email)]
	if !ok {
		return password.User{}, password.ErrUserNotFound
	}
	return u, nil
}

func (s *memStore) Create(_ context.Context, email string, hash []byte) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = strings.ToLower(email)
	if _, exists := s.users[email]; exists {
		return password.User{}, password.ErrEmailTaken
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	u := password.User{ID: hex.EncodeToString(b[:]), Email: email, PassHash: hash}
	s.users[email] = u
	return u, nil
}

func (s *memStore) SetPassword(_ context.Context, userID string, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, u := range s.users {
		if u.ID == userID {
			u.PassHash = hash
			s.users[k] = u
			return nil
		}
	}
	return password.ErrUserNotFound
}

// --- fixture (identity-only session payload) ---

// defaultPrefix is the prefix the fixture mounts the provider at.
// Tests build full paths via defaultPrefix + f.path(password.PathLogin) etc.
const defaultPrefix = "/auth/user"

// The default fixture uses *session.Manager[auth.Identity] — the
// simplest layout, where the session payload IS the identity and
// *auth.Identity satisfies auth.Identifiable via the method on the
// auth package itself.
type fixture struct {
	store   *memStore
	sessMgr *session.Manager[auth.Identity]
	ph      *password.Provider[auth.Identity]
	router  *router.Router
	prefix  string
}

func newFixture(t *testing.T, opts password.Options) *fixture {
	t.Helper()
	store := newMemStore()
	sessMgr, err := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ph := password.New(store, sessMgr, opts)

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes(defaultPrefix, r)
	return &fixture{store: store, sessMgr: sessMgr, ph: ph, router: r, prefix: defaultPrefix}
}

// path joins the fixture's prefix with one of the password.Path*
// suffixes — keeps tests free of hardcoded full URLs.
func (f *fixture) path(suffix string) string { return f.prefix + suffix }

func (f *fixture) seed(t *testing.T, email, plain string) password.User {
	t.Helper()
	hash, err := password.BcryptHasher{Cost: 4}.Hash(plain)
	if err != nil {
		t.Fatalf("seed hash: %v", err)
	}
	u, err := f.store.Create(context.Background(), email, hash)
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	return u
}

func (f *fixture) postJSON(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// sessionPayload reads the current session payload for the supplied
// cookies — used to verify what password actually wrote.
func (f *fixture) sessionPayload(t *testing.T, cookies []*http.Cookie) (auth.Identity, bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	var got auth.Identity
	var ok bool
	probe := router.New()
	probe.Use(f.sessMgr.Middleware)
	probe.Get("/probe", func(w http.ResponseWriter, r *http.Request) {
		p, err := f.sessMgr.Get(r.Context())
		if err == nil && p != nil && p.Subject != "" {
			got = *p
			ok = true
		}
	})
	probe.ServeHTTP(httptest.NewRecorder(), req)
	return got, ok
}

// --- constructor discipline ---

func TestNew_NilStorePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil Store")
		}
	}()
	mgr, _ := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	_ = password.New(nil, mgr, password.Options{})
}

func TestNew_NilSessionManagerPanics(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic on nil session.Manager")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, "session.Manager") {
			t.Errorf("panic = %v, want one mentioning session.Manager", v)
		}
	}()
	_ = password.New[auth.Identity](newMemStore(), nil, password.Options{})
}

// rejectsEmptyHasher is a Hasher implementation that fails on empty
// plaintext — the contract violation the package's "Hash MUST accept
// empty" doc note exists to prevent. password.New mints a timing-
// safety dummy via Hash("") at boot; if Hash rejects empty, New must
// panic loudly rather than silently fall back to anything that would
// resurrect the timing leak.
type rejectsEmptyHasher struct{}

func (rejectsEmptyHasher) Hash(plain string) ([]byte, error) {
	if plain == "" {
		return nil, errors.New("rejectsEmptyHasher: empty input not allowed")
	}
	return []byte("ok"), nil
}

func (rejectsEmptyHasher) Verify(string, []byte) bool { return false }

// TestNew_HasherRejectsEmpty_PanicsClearly pins the Hasher contract
// documented on Hasher.Hash: implementations MUST accept an empty
// plain string. password.New calls Hash("") at boot to mint the
// timing-safety dummy; a Hasher that rejects empty input causes a
// boot-time panic with a message naming "dummy hash" so the operator
// can connect the failure to the contract violation.
func TestNew_HasherRejectsEmpty_PanicsClearly(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic when configured Hasher rejects Hash(\"\")")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, "dummy") {
			t.Errorf("panic = %q, want mention of 'dummy' so the operator can connect the failure to the documented contract",
				msg)
		}
	}()
	_ = password.New(newMemStore(), mustSessionMgr(t), password.Options{
		Hasher: rejectsEmptyHasher{},
	})
}

// --- ProviderName ---

// TestProviderName_Value pins the wire-level value of
// password.ProviderName. Apps may persist Identity.Provider in audit
// logs, analytics rows, or row-level access rules — changing the
// string silently breaks those without warning, so the constant's
// value is part of the public contract.
func TestProviderName_Value(t *testing.T) {
	if password.ProviderName != "password" {
		t.Errorf("password.ProviderName = %q, want %q — value is part of the public contract", password.ProviderName, "password")
	}
}

// TestLogin_DoesNotSetIdentityName pins that the password backend
// never populates Identity.Name. The Store has no Name column and
// this backend has no notion of a display name; apps that want one
// look it up from their own user table by Subject. Anyone adding a
// Name column to User and threading it into identityFor without
// also documenting the new contract will fail this test.
func TestLogin_DoesNotSetIdentityName(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "correct-password")

	rec := f.postJSON(t, f.path(password.PathLogin),
		`{"email":"alice@example.com","password":"correct-password"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login code = %d, want 204", rec.Code)
	}

	got, ok := f.sessionPayload(t, rec.Result().Cookies())
	if !ok {
		t.Fatal("no identity stored in session after login")
	}
	if got.Name != "" {
		t.Errorf("Identity.Name = %q, want \"\" — password backend must not populate Name", got.Name)
	}
}

// --- login ---

func TestLogin_SuccessWritesSessionAnd204(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	u := f.seed(t, "alice@example.com", "correct horse battery staple")

	rec := f.postJSON(t, f.path(password.PathLogin), `{"email":"alice@example.com","password":"correct horse battery staple"}`)
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	got, ok := f.sessionPayload(t, cookies)
	if !ok {
		t.Fatal("no identity stored in session after login")
	}
	if got.Subject != u.ID {
		t.Errorf("Subject = %q, want %q", got.Subject, u.ID)
	}
	if got.Provider != password.ProviderName {
		t.Errorf("Provider = %q, want %q", got.Provider, password.ProviderName)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("Email = %q", got.Email)
	}
}

func TestLogin_WrongPassword_401AndNoSession(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "correct password")

	rec := f.postJSON(t, f.path(password.PathLogin), `{"email":"alice@example.com","password":"WRONG"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if _, ok := f.sessionPayload(t, rec.Result().Cookies()); ok {
		t.Error("session was written on failed login")
	}
}

func TestLogin_UnknownEmail_401AndNoSession(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	rec := f.postJSON(t, f.path(password.PathLogin), `{"email":"nobody@example.com","password":"anything"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if _, ok := f.sessionPayload(t, rec.Result().Cookies()); ok {
		t.Error("session was written on unknown-email login")
	}
}

// TestLogin_UnknownEmail_RunsDummyHashCheck pins that the unknown-
// email login path runs a bcrypt Verify against a dummy hash at the
// SAME cost as real stored hashes. A cost-mismatched dummy (the
// previous package-level constant was cost 4 while the default
// Hasher is cost 12) returned ~10x faster than a real failed
// compare, leaking "this email is not registered" to a timing
// attacker.
//
// The assertion is a tight ratio band centred on 1.0. The previous
// test used a loose "unknown >= 30% of wrong" check which masked the
// cost-4-vs-cost-12 gap; this band would fail under that gap.
//
// Run multiple iterations and take the median to dampen scheduler
// jitter. The band is generous enough to survive CI variance but
// tight enough that a real cost mismatch trips it.
func TestLogin_UnknownEmail_RunsDummyHashCheck(t *testing.T) {
	const iterations = 5
	const cost = 8 // ~50ms per Verify on commodity hardware — fast enough for CI

	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: cost}})

	// Seed at the SAME cost the Provider's Hasher uses, otherwise
	// the wrong-password path verifies against a cost-4 (seed
	// default) hash while the unknown-email path verifies against
	// the per-cost dummy — making the two paths incomparable and
	// invalidating the timing assertion.
	hash, err := password.BcryptHasher{Cost: cost}.Hash("real-password-123456")
	if err != nil {
		t.Fatalf("seed hash: %v", err)
	}
	if _, err := f.store.Create(context.Background(), "alice@example.com", hash); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	measure := func(body string) time.Duration {
		ds := make([]time.Duration, 0, iterations)
		for i := 0; i < iterations; i++ {
			start := time.Now()
			_ = f.postJSON(t, f.path(password.PathLogin), body)
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2] // median
	}

	unknown := measure(`{"email":"nobody@example.com","password":"any"}`)
	wrong := measure(`{"email":"alice@example.com","password":"WRONG"}`)

	// Ratio should be close to 1: both paths run one bcrypt Verify
	// at the same cost. Tolerate 2x in either direction for CI
	// jitter; a 10x gap (the cost-4-vs-cost-12 bug) blows through.
	ratio := float64(unknown) / float64(wrong)
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("unknown/wrong timing ratio = %.2f (unknown=%v wrong=%v), want close to 1.0 — dummy hash cost may not match configured Hasher cost",
			ratio, unknown, wrong)
	}
}

// TestLogin_UnknownEmail_DummyTracksConfiguredCost pins the
// per-cost-match contract from a different angle: at an explicitly
// higher cost (10 vs the default 12), the unknown-email path should
// take time proportional to that cost — not the cost-4 floor the
// old hardcoded dummy used.
//
// Asserts an absolute floor on unknown-email latency. At cost 10
// (~150ms per Verify) we expect well over 50ms even on slow CI. The
// old cost-4 dummy returned in ~25ms, so this floor catches the
// regression where a builder caches a cost-4 dummy across hasher
// configurations.
func TestLogin_UnknownEmail_DummyTracksConfiguredCost(t *testing.T) {
	const cost = 10 // ~150ms per Verify; deliberately higher than the test default
	const minExpected = 50 * time.Millisecond

	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: cost}})

	start := time.Now()
	_ = f.postJSON(t, f.path(password.PathLogin),
		`{"email":"nobody@example.com","password":"any"}`)
	unknown := time.Since(start)

	if unknown < minExpected {
		t.Errorf("unknown-email login took %v at cost %d — under the %v floor; dummy hash is likely not being computed at the configured cost",
			unknown, cost, minExpected)
	}
}

func TestLogin_MissingFields_401(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	rec := f.postJSON(t, f.path(password.PathLogin), `{"email":"","password":""}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestLogin_MalformedBody_401(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogin), strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

// --- body-size cap ---

// TestLogin_OversizedBody_RejectedViaOnFailure pins that an oversized
// request body is rejected before reaching bcrypt or the store. A
// custom OnFailure captures the parse error so we can confirm it came
// from http.MaxBytesReader and not from a downstream failure that
// might have done expensive work.
func TestLogin_OversizedBody_RejectedViaOnFailure(t *testing.T) {
	var seen error
	f := newFixture(t, password.Options{
		Hasher:       password.BcryptHasher{Cost: 4},
		MaxBodyBytes: 64, // intentionally tiny
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "too large", http.StatusUnauthorized)
		},
	})
	// Seed a real user — we want to be sure the rejection happens
	// before any user lookup or hash compare.
	f.seed(t, "alice@example.com", "real-password")

	bigBody := `{"email":"alice@example.com","password":"` + strings.Repeat("x", 4000) + `"}`
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogin), strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if seen == nil {
		t.Fatal("OnFailure did not see an error")
	}
	if !strings.Contains(seen.Error(), "parse") {
		t.Errorf("OnFailure saw %v, want a parse error (the body cap surfaces through Parser)", seen)
	}
}

// TestLogin_BodyWithinDefaultCap_Succeeds ensures the default 64 KiB
// cap is generous enough for legitimate logins. A realistic-sized
// JSON body (well under 64 KiB) must continue to authenticate.
func TestLogin_BodyWithinDefaultCap_Succeeds(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "correct-password")

	rec := f.postJSON(t, f.path(password.PathLogin),
		`{"email":"alice@example.com","password":"correct-password"}`)
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204 — default body cap should not reject normal payloads", rec.Code)
	}
}

// TestRegister_OversizedBody_Rejected mirrors the login test on the
// register handler so the cap is enforced on both code paths.
func TestRegister_OversizedBody_Rejected(t *testing.T) {
	f := newFixture(t, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
		MaxBodyBytes:    64,
	})

	bigBody := `{"email":"new@example.com","password":"` + strings.Repeat("x", 4000) + `"}`
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathRegister), strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401 — register must reject oversized bodies too", rec.Code)
	}
	// The user must not have been created.
	if _, err := f.store.LookupByEmail(context.Background(), "new@example.com"); err == nil {
		t.Error("user was created from an oversized register body — cap must run before Store.Create")
	}
}

// TestLogin_DefaultMaxBodyBytes_Applies pins that a zero
// Options.MaxBodyBytes falls back to DefaultMaxBodyBytes (not zero,
// which would reject every request).
func TestLogin_DefaultMaxBodyBytes_Applies(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "correct-password")

	// 60 KiB body — fits under 64 KiB default, exceeds any plausible
	// "0 means unconfigured" interpretation.
	pad := strings.Repeat("p", 60<<10)
	body := `{"email":"alice@example.com","password":"correct-password","_pad":"` + pad + `"}`
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogin), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204 — default cap (64 KiB) must allow a 60 KiB body through", rec.Code)
	}
}

// --- Provider as auth.Authenticator ---

func TestProvider_Authenticate_NoSessionReturnsUnauthenticated(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	// Request that did not go through sessMgr.Middleware.
	_, err := f.ph.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestLogin_PromoteRotatesSID pins that login rotates the SID. Without
// rotation the login is fixation-prone: an attacker who plants a
// known SID on the victim's browser would end up sharing the session
// post-login. The test sets a pre-existing session cookie, performs a
// login, and asserts the response cookie carries a different SID.
//
// Combined with the Promote-before-Update ordering in writeSession,
// this also pins the bug-fix invariant: even if Update were to fail
// after Promote succeeded, the request-end commit would still rotate
// the SID, so no identity ever lands on the attacker-controlled SID.
func TestLogin_PromoteRotatesSID(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "correct-password")

	// Round 1: a request that just touches the session, to get a
	// "pre-login" SID minted. We don't write anything — this models
	// the attacker pre-planting a cookie on the victim.
	preReq := httptest.NewRequest(http.MethodGet, "/anything", nil)
	preRec := httptest.NewRecorder()
	preR := router.New()
	preR.Use(f.sessMgr.Middleware)
	preR.Get("/anything", func(w http.ResponseWriter, r *http.Request) {
		// Force the session to commit by writing something.
		_ = f.sessMgr.Save(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	preR.ServeHTTP(preRec, preReq)
	preCookies := preRec.Result().Cookies()
	if len(preCookies) == 0 {
		t.Fatal("pre-login request did not set a cookie")
	}
	preSID := preCookies[0].Value

	// Round 2: login with the pre-login cookie. Promote MUST rotate
	// the SID so the response carries a different cookie value.
	loginReq := httptest.NewRequest(http.MethodPost, f.path(password.PathLogin),
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	for _, c := range preCookies {
		loginReq.AddCookie(c)
	}
	loginRec := httptest.NewRecorder()
	f.router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusNoContent {
		t.Fatalf("login code = %d, want 204", loginRec.Code)
	}

	postCookies := loginRec.Result().Cookies()
	if len(postCookies) == 0 {
		t.Fatal("login did not set a post-login cookie")
	}
	postSID := postCookies[0].Value

	if postSID == preSID {
		t.Errorf("SID was not rotated on login: pre=%q post=%q — Promote must run as part of writeSession to foreclose session fixation",
			preSID, postSID)
	}

	// Belt-and-braces: the pre-login SID must no longer carry an
	// identity. If it does, the rotation didn't actually move the
	// identity to the new SID.
	if id, ok := f.sessionPayload(t, preCookies); ok {
		t.Errorf("pre-login SID %q still resolves to identity %+v — Promote/rotation incomplete",
			preSID, id)
	}
}

// TestLogin_PromoteFailure_AbortsCleanly is the bug-1 failure-mode
// pin. With the Promote-first ordering in writeSession, a Promote
// failure must abort BEFORE any identity-bearing payload mutation
// happens. We force the failure by mounting the provider on a
// router that does NOT include sessMgr.Middleware; Promote then
// returns session.ErrNoSession (no per-request state attached).
//
// The invariants this test pins:
//   - OnFailure is invoked with an error wrapped "session:" so apps
//     can distinguish from credential failures.
//   - No Set-Cookie is written — no session was created or rotated.
//   - The user store has the seeded user, but no in-memory session
//     store entry exists naming that user. The failure is total.
//
// If someone re-introduces the buggy Update-first ordering, this
// test still detects the divergence: the session is dirty when
// Update succeeds, so the request-end commit would try to fire and
// either panic (no state) or surface a different error than the
// "session:" wrap. Either way the strict assertions below fail.
func TestLogin_PromoteFailure_AbortsCleanly(t *testing.T) {
	store := newMemStore()
	sessMgr := mustSessionMgr(t)

	var seen error
	ph := password.New(store, sessMgr, password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "login failed", http.StatusUnauthorized)
		},
	})

	// Seed a real user — confirm we'd succeed on the credential path
	// if Promote didn't fail. This pins that the abort is structural,
	// not a parse/lookup/hash issue.
	hash, _ := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	if _, err := store.Create(context.Background(), "alice@example.com", hash); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	// CRUCIAL: router does NOT use sessMgr.Middleware.
	r := router.New()
	ph.RegisterRoutes(defaultPrefix, r)

	req := httptest.NewRequest(http.MethodPost, defaultPrefix+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if seen == nil {
		t.Fatal("OnFailure was not invoked — handleLogin must surface Promote failure through the hook")
	}
	if !strings.Contains(seen.Error(), "session:") {
		t.Errorf("OnFailure error = %q, want it wrapped with \"session:\" (writeSession's wrap)", seen.Error())
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401 (custom OnFailure ran)", rec.Code)
	}

	// No cookie may have been set — that would mean a session was
	// minted and committed despite the failure.
	if cookies := rec.Result().Cookies(); len(cookies) > 0 {
		t.Errorf("response set %d cookie(s) despite Promote failure: %+v — partial session was committed",
			len(cookies), cookies)
	}

	// Independent observation: no identity should resolve for any
	// SID that might have been generated. A handcrafted GET that
	// goes through Middleware proves the in-memory store contains
	// no identity-bearing session.
	probeReq := httptest.NewRequest(http.MethodGet, "/probe", nil)
	for _, c := range rec.Result().Cookies() {
		probeReq.AddCookie(c)
	}
	probe := router.New()
	probe.Use(sessMgr.Middleware)
	var probeID auth.Identity
	var haveID bool
	probe.Get("/probe", func(w http.ResponseWriter, r *http.Request) {
		p, err := sessMgr.Get(r.Context())
		if err == nil && p != nil && p.Subject != "" {
			probeID = *p
			haveID = true
		}
	})
	probe.ServeHTTP(httptest.NewRecorder(), probeReq)
	if haveID {
		t.Errorf("session store carries an identity after a failed login: %+v — Promote-first ordering broken",
			probeID)
	}
}

// TestLogin_Concurrent_NoRace is a smoke test under -race for
// concurrent login traffic. N goroutines each log in as a distinct
// user against the shared provider. The intent is to catch any
// internal mutable state Provider accidentally shares across
// requests (counters, caches, defaults set lazily, etc.).
//
// The session library's per-request state attachment via Middleware
// is what isolates concurrent requests; this test pins that the
// password layer doesn't add any cross-request sharing that would
// trip the race detector or corrupt sessions.
func TestLogin_Concurrent_NoRace(t *testing.T) {
	const N = 32

	f := newFixture(t, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
	})

	// Seed N distinct users so each goroutine has its own happy path.
	for i := 0; i < N; i++ {
		email := emailFor(i)
		hash, err := password.BcryptHasher{Cost: 4}.Hash("correct-password")
		if err != nil {
			t.Fatalf("seed hash: %v", err)
		}
		if _, err := f.store.Create(context.Background(), email, hash); err != nil {
			t.Fatalf("seed create: %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, N)
	cookies := make(chan *http.Cookie, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			email := emailFor(i)
			body := `{"email":"` + email + `","password":"correct-password"}`
			req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogin), strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				errs <- fmt.Errorf("user %d login code = %d, want 204", i, rec.Code)
				return
			}
			cs := rec.Result().Cookies()
			if len(cs) == 0 {
				errs <- fmt.Errorf("user %d login set no cookie", i)
				return
			}
			cookies <- cs[0]
		}(i)
	}
	wg.Wait()
	close(errs)
	close(cookies)

	for err := range errs {
		t.Error(err)
	}

	// Each login must have minted a DISTINCT SID. Sharing would
	// indicate a session-state leak across concurrent requests.
	seen := make(map[string]bool, N)
	for c := range cookies {
		if seen[c.Value] {
			t.Errorf("SID %q appeared in two concurrent logins — per-request session isolation broken", c.Value)
		}
		seen[c.Value] = true
	}
	if len(seen) == 0 {
		t.Fatal("no cookies collected — all logins failed")
	}
}

// emailFor is a tiny helper used only by the concurrent test to
// generate per-goroutine identities.
func emailFor(i int) string { return fmt.Sprintf("u%d@example.com", i) }

func TestProvider_Authenticate_ReadsSessionAfterLogin(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	u := f.seed(t, "alice@example.com", "correct-password")

	login := f.postJSON(t, f.path(password.PathLogin), `{"email":"alice@example.com","password":"correct-password"}`)
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}

	// Second request — should be identified via the session cookie.
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}

	var got auth.Identity
	var gotErr error
	probe := router.New()
	probe.Use(f.sessMgr.Middleware)
	probe.Get("/me", func(w http.ResponseWriter, r *http.Request) {
		got, gotErr = f.ph.Authenticate(r)
	})
	probe.ServeHTTP(httptest.NewRecorder(), req)

	if gotErr != nil {
		t.Fatalf("Authenticate: %v", gotErr)
	}
	if got.Subject != u.ID {
		t.Errorf("Subject = %q, want %q", got.Subject, u.ID)
	}
	if got.Provider != password.ProviderName {
		t.Errorf("Provider = %q, want %q", got.Provider, password.ProviderName)
	}
}

// --- register ---

func TestRegister_DisabledByDefault_404Or405(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	rec := f.postJSON(t, f.path(password.PathRegister), `{"email":"new@example.com","password":"longenough"}`)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 404 or 405 (register not enabled)", rec.Code)
	}
}

func TestRegister_EnabledHappyPath_AutoLogin204(t *testing.T) {
	f := newFixture(t, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
	})

	rec := f.postJSON(t, f.path(password.PathRegister), `{"email":"new@example.com","password":"longenough"}`)
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	if _, ok := f.sessionPayload(t, rec.Result().Cookies()); !ok {
		t.Error("expected auto-login session after register")
	}
}

func TestRegister_PasswordTooShort_400(t *testing.T) {
	f := newFixture(t, password.Options{
		RegisterEnabled:   true,
		MinPasswordLength: 12,
		Hasher:            password.BcryptHasher{Cost: 4},
	})

	rec := f.postJSON(t, f.path(password.PathRegister), `{"email":"new@example.com","password":"short"}`)
	// Default OnFailure returns 400 (Bad Request) for the
	// password-too-short policy rejection — distinct from 401 used
	// for credential failures. Policy rejection ≠ auth failure.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 — password-too-short is a policy rejection, not a credential failure", rec.Code)
	}
	if _, ok := f.sessionPayload(t, rec.Result().Cookies()); ok {
		t.Error("session was written despite too-short password")
	}
}

// TestRegister_TooShort_ErrorIsSentinel pins that the password-too-short
// error reaching OnFailure satisfies errors.Is(err, ErrPasswordTooShort).
// Lets apps branch on the cause without parsing strings.
func TestRegister_TooShort_ErrorIsSentinel(t *testing.T) {
	var seen error
	f := newFixture(t, password.Options{
		RegisterEnabled:   true,
		MinPasswordLength: 12,
		Hasher:            password.BcryptHasher{Cost: 4},
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "nope", http.StatusUnauthorized)
		},
	})

	_ = f.postJSON(t, f.path(password.PathRegister),
		`{"email":"new@example.com","password":"short"}`)

	if seen == nil {
		t.Fatal("OnFailure did not see an error")
	}
	if !errors.Is(seen, password.ErrPasswordTooShort) {
		t.Errorf("errors.Is(err, ErrPasswordTooShort) = false; err = %v", seen)
	}
}

// TestRegister_TooShort_ErrorCarriesMinViaErrorsAs pins that
// errors.As extracts a *PasswordTooShortError carrying the configured
// minimum length. This is the supported way for a custom OnFailure
// to render the policy in its UI without depending on Options.
func TestRegister_TooShort_ErrorCarriesMinViaErrorsAs(t *testing.T) {
	var seen error
	f := newFixture(t, password.Options{
		RegisterEnabled:   true,
		MinPasswordLength: 12,
		Hasher:            password.BcryptHasher{Cost: 4},
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "nope", http.StatusUnauthorized)
		},
	})

	_ = f.postJSON(t, f.path(password.PathRegister),
		`{"email":"new@example.com","password":"short"}`)

	var pse *password.PasswordTooShortError
	if !errors.As(seen, &pse) {
		t.Fatalf("errors.As(seen, *PasswordTooShortError) = false; err = %v", seen)
	}
	if pse.Min != 12 {
		t.Errorf("PasswordTooShortError.Min = %d, want 12", pse.Min)
	}
}

// TestRegister_TooShort_ErrorMessageDoesNotLeakMin pins the
// information-disclosure discipline: a handler that surfaces
// err.Error() to the client must NOT see the configured minimum
// embedded in the message. The minimum is available via errors.As
// only.
func TestRegister_TooShort_ErrorMessageDoesNotLeakMin(t *testing.T) {
	var seen error
	f := newFixture(t, password.Options{
		RegisterEnabled:   true,
		MinPasswordLength: 17, // a specific, recognisable number
		Hasher:            password.BcryptHasher{Cost: 4},
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "nope", http.StatusUnauthorized)
		},
	})

	_ = f.postJSON(t, f.path(password.PathRegister),
		`{"email":"new@example.com","password":"short"}`)

	if seen == nil {
		t.Fatal("OnFailure did not see an error")
	}
	msg := seen.Error()
	if strings.Contains(msg, "17") {
		t.Errorf("Error() = %q leaks the configured minimum (17) — must be carried only in the typed error", msg)
	}
	// Belt-and-braces: the message is the sentinel's message.
	if msg != password.ErrPasswordTooShort.Error() {
		t.Errorf("Error() = %q, want %q (ErrPasswordTooShort.Error())", msg, password.ErrPasswordTooShort.Error())
	}
}

// TestRegister_NegativeMinPasswordLength_FallsBackToDefault pins the
// alignment with MaxBodyBytes: a negative MinPasswordLength must NOT
// silently disable the minimum (len(pwd) < -5 is always false, which
// would accept any non-empty password). It must fall back to the
// default 8 — same defensive default as zero.
func TestRegister_NegativeMinPasswordLength_FallsBackToDefault(t *testing.T) {
	f := newFixture(t, password.Options{
		RegisterEnabled:   true,
		MinPasswordLength: -5, // nonsensical; must NOT disable enforcement
		Hasher:            password.BcryptHasher{Cost: 4},
	})

	// 5-char password — under the default 8. If the negative value
	// silently disabled enforcement, this would succeed (204).
	rec := f.postJSON(t, f.path(password.PathRegister),
		`{"email":"new@example.com","password":"short"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 — negative MinPasswordLength must fall back to the default 8 (policy rejection → 400), not disable enforcement",
			rec.Code)
	}
	if _, ok := f.sessionPayload(t, rec.Result().Cookies()); ok {
		t.Error("session was written despite too-short password under negative-MinPasswordLength fallback")
	}

	// Sanity: an 8-char password must be accepted (proves the
	// default kicked in, not some larger value).
	rec = f.postJSON(t, f.path(password.PathRegister),
		`{"email":"other@example.com","password":"exactly8"}`)
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204 for 8-char password under default-8 fallback", rec.Code)
	}
}

func TestRegister_EmailTaken_401(t *testing.T) {
	f := newFixture(t, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
	})
	f.seed(t, "taken@example.com", "existing-password")

	rec := f.postJSON(t, f.path(password.PathRegister), `{"email":"taken@example.com","password":"newpassword"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestRegister_CustomOnRegistered_NoAutoLogin(t *testing.T) {
	called := false
	var seenUser password.User
	f := newFixture(t, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
		OnRegistered: func(w http.ResponseWriter, r *http.Request, u password.User) {
			called = true
			seenUser = u
			w.WriteHeader(http.StatusAccepted)
		},
	})

	rec := f.postJSON(t, f.path(password.PathRegister), `{"email":"new@example.com","password":"longenough"}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202 (custom OnRegistered)", rec.Code)
	}
	if !called {
		t.Error("custom OnRegistered was not called")
	}
	if seenUser.Email != "new@example.com" {
		t.Errorf("User.Email = %q", seenUser.Email)
	}
	if _, ok := f.sessionPayload(t, rec.Result().Cookies()); ok {
		t.Error("session was written despite custom OnRegistered skipping auto-login")
	}
}

// --- default OnFailure wire shapes ---

// TestDefaultOnFailure_LogoutFailure_500 pins that the default
// OnFailure returns 500 with a "logout failed" body when handleLogout
// fails — NOT 401 "invalid credentials" (the user's credentials had
// nothing to do with it; the session destroy failed).
//
// Forced by mounting without sessMgr.Middleware so Destroy returns
// session.ErrNoSession, the same surface a real store outage would
// produce.
func TestDefaultOnFailure_LogoutFailure_500(t *testing.T) {
	prov := password.New(newMemStore(), mustSessionMgr(t), password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
	})
	r := router.New() // NO sessMgr.Middleware
	prov.RegisterRoutes(defaultPrefix, r)

	req := httptest.NewRequest(http.MethodPost, defaultPrefix+password.PathLogout, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500 — logout failure must not collapse to 401 invalid credentials", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "logout failed" {
		t.Errorf("body = %q, want \"logout failed\"", body)
	}
}

// TestDefaultOnFailure_SessionWriteFailure_500 pins that the default
// returns 500 with a "session error" body when writeSession fails
// during a login. Mounted without sessMgr.Middleware so the writeSession
// Promote step fails — the surface a session store outage would produce.
func TestDefaultOnFailure_SessionWriteFailure_500(t *testing.T) {
	store := newMemStore()
	prov := password.New(store, mustSessionMgr(t), password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
	})

	// Seed so credentials succeed; the failure point is writeSession.
	hash, _ := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	_, _ = store.Create(context.Background(), "alice@example.com", hash)

	r := router.New() // NO sessMgr.Middleware
	prov.RegisterRoutes(defaultPrefix, r)

	req := httptest.NewRequest(http.MethodPost, defaultPrefix+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500 — session write failure must not collapse to 401", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "session error" {
		t.Errorf("body = %q, want \"session error\"", body)
	}
}

// TestDefaultOnFailure_EmailTaken_Still401 pins that the default
// keeps ErrEmailTaken collapsing to a generic 401 — registration
// failures still must not telegraph "this email is registered" to a
// probing client (timing-safe enumeration discipline extends to
// register).
func TestDefaultOnFailure_EmailTaken_Still401(t *testing.T) {
	f := newFixture(t, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
	})
	f.seed(t, "taken@example.com", "existing-password")

	rec := f.postJSON(t, f.path(password.PathRegister),
		`{"email":"taken@example.com","password":"newpassword"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401 — ErrEmailTaken must collapse to generic 401 to avoid leaking registration state",
			rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "invalid credentials" {
		t.Errorf("body = %q, want \"invalid credentials\" — must not leak \"email taken\"", body)
	}
}

// TestDefaultOnFailure_BadCredentials_401 sanity-pins the credential
// path's wire shape so the routing in defaultOnFailure can't silently
// drop the 401 default while the other branches are being landed.
func TestDefaultOnFailure_BadCredentials_401(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "correct-password")

	rec := f.postJSON(t, f.path(password.PathLogin),
		`{"email":"alice@example.com","password":"WRONG"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "invalid credentials" {
		t.Errorf("body = %q, want \"invalid credentials\"", body)
	}
}

// --- logout ---

func TestLogout_DestroysSessionAnd204(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})
	f.seed(t, "alice@example.com", "p4ssw0rd")

	// Login.
	login := f.postJSON(t, f.path(password.PathLogin), `{"email":"alice@example.com","password":"p4ssw0rd"}`)
	cookies := login.Result().Cookies()

	// Logout.
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogout), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	if _, ok := f.sessionPayload(t, cookies); ok {
		t.Error("session still readable after logout")
	}
}

// TestLogout_AnonymousRequest_204 pins the wire shape for logging out
// when the client has no inbound cookie: a real scenario when a user
// double-clicks logout, the cookie expired between page load and
// click, or someone hits POST /auth/user/logout directly.
//
// session.Manager.Destroy marks the (empty) per-request state for
// destruction without inspecting whether a session exists, so
// handleLogout's Destroy returns nil and the configured OnSuccess
// fires — same wire shape as a logout that destroyed real state.
// That's the only sensible behavior: a fresh client should not see
// a 5xx for asking "log me out, please".
//
// Anyone who later changes session.Manager.Destroy to return an
// error on no-session-present, or adds a pre-check to handleLogout,
// will break this test.
func TestLogout_AnonymousRequest_204(t *testing.T) {
	f := newFixture(t, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	// No login first. Cookie jar is empty.
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogout), nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("anonymous logout code = %d, want 204 — POST logout without a session must be a successful no-op",
			rec.Code)
	}
}

// TestLogout_RoutesThroughOnSuccess_WithEmptyIdentity pins the
// contract that logout success goes through the configurable
// OnSuccess hook with the zero Identity as the signal that this is a
// logout context (not a login). Apps overriding OnSuccess for
// HTMX/redirect responses get their handler invoked instead of a
// hard-coded 204.
func TestLogout_RoutesThroughOnSuccess_WithEmptyIdentity(t *testing.T) {
	var calls int
	var seenID auth.Identity
	f := newFixture(t, password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
		OnSuccess: func(w http.ResponseWriter, r *http.Request, id auth.Identity) {
			calls++
			seenID = id
			// Distinct status code so the test can confirm THIS
			// handler ran instead of the default 204.
			http.Redirect(w, r, "/goodbye", http.StatusSeeOther)
		},
	})
	f.seed(t, "alice@example.com", "p4ssw0rd")
	login := f.postJSON(t, f.path(password.PathLogin), `{"email":"alice@example.com","password":"p4ssw0rd"}`)
	if calls != 1 {
		t.Fatalf("login should have invoked OnSuccess once; got calls=%d", calls)
	}
	if seenID.Subject == "" {
		t.Fatal("login OnSuccess saw empty Subject — login must populate Identity")
	}

	// Reset and exercise the logout path.
	calls = 0
	seenID = auth.Identity{Subject: "should-be-overwritten"}
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogout), nil)
	for _, c := range login.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if calls != 1 {
		t.Fatalf("logout did not invoke OnSuccess; got calls=%d — handleLogout must route success through the hook", calls)
	}
	if seenID.Subject != "" {
		t.Errorf("logout OnSuccess saw Subject=%q, want \"\" — empty Identity is the logout-context signal", seenID.Subject)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("logout response code = %d, want 303 from the custom OnSuccess; hook was bypassed", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/goodbye" {
		t.Errorf("Location = %q, want /goodbye", loc)
	}
}

// TestLogout_RoutesThroughOnFailure_OnDestroyError pins the failure
// counterpart: a Destroy error reaches the configurable OnFailure
// (wrapped with "logout:" so handlers can distinguish from other
// causes) instead of a raw 500 written out of band.
//
// To force a Destroy error we POST logout without going through
// sessMgr.Middleware — Destroy then returns session.ErrNoSession
// because there's no per-request state attached. That's the same
// failure shape a production store outage would surface.
func TestLogout_RoutesThroughOnFailure_OnDestroyError(t *testing.T) {
	var seen error
	ph := password.New(newMemStore(), mustSessionMgr(t), password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "logout broke", http.StatusBadGateway)
		},
	})

	// Build a router WITHOUT sessMgr.Middleware — Destroy will fail
	// with ErrNoSession.
	r := router.New()
	ph.RegisterRoutes(defaultPrefix, r)

	req := httptest.NewRequest(http.MethodPost, defaultPrefix+password.PathLogout, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if seen == nil {
		t.Fatal("OnFailure was not called — handleLogout must route Destroy errors through the hook")
	}
	if !strings.Contains(seen.Error(), "logout:") {
		t.Errorf("OnFailure error = %q, want it wrapped with \"logout:\" so handlers can distinguish causes", seen.Error())
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("response code = %d, want 502 from the custom OnFailure; raw 500 was written instead", rec.Code)
	}
}

// mustSessionMgr is a small helper used by the no-Middleware logout
// test above; the existing fixture wires sessMgr.Middleware, which
// we need to skip for this one case.
func mustSessionMgr(t *testing.T) *session.Manager[auth.Identity] {
	t.Helper()
	mgr, err := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	return mgr
}

// --- customisation hooks ---

func TestCustomOnSuccess_RedirectsLikeServerRenderedApp(t *testing.T) {
	f := newFixture(t, password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
		OnSuccess: func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		},
	})
	f.seed(t, "alice@example.com", "correct-password")

	rec := f.postJSON(t, f.path(password.PathLogin), `{"email":"alice@example.com","password":"correct-password"}`)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("code = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("Location = %q, want /dashboard", loc)
	}
}

func TestCustomOnFailure_SeesOriginalError(t *testing.T) {
	var seen error
	f := newFixture(t, password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
		OnFailure: func(w http.ResponseWriter, _ *http.Request, err error) {
			seen = err
			http.Error(w, "nope", http.StatusUnauthorized)
		},
	})

	_ = f.postJSON(t, f.path(password.PathLogin), `{"email":"x@example.com","password":"y"}`)
	if seen == nil {
		t.Fatal("OnFailure did not see an error")
	}
	if !errors.Is(seen, password.ErrInvalidCredentials) {
		t.Errorf("OnFailure saw %v, want ErrInvalidCredentials", seen)
	}
}

func TestCustomParser_AcceptsAlternateFieldNames(t *testing.T) {
	f := newFixture(t, password.Options{
		Hasher: password.BcryptHasher{Cost: 4},
		Parser: func(r *http.Request) (password.Credentials, error) {
			if err := r.ParseForm(); err != nil {
				return password.Credentials{}, err
			}
			return password.Credentials{
				Email:    r.PostFormValue("username"),
				Password: r.PostFormValue("passwd"),
			}, nil
		},
	})
	f.seed(t, "alice@example.com", "real")

	body := bytes.NewBufferString("username=alice@example.com&passwd=real")
	req := httptest.NewRequest(http.MethodPost, f.path(password.PathLogin), body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
}

// --- prefix handling ---

// TestRegisterRoutes_CustomPrefix proves the routes mount at a
// caller-supplied prefix, not at the hardcoded "/auth/user". Pins the
// signature ph.RegisterRoutes(prefix, r) and the prefix+suffix
// composition.
func TestRegisterRoutes_CustomPrefix(t *testing.T) {
	const customPrefix = "/api/v2/identity"

	store := newMemStore()
	sessMgr, err := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ph := password.New(store, sessMgr, password.Options{
		RegisterEnabled: true,
		Hasher:          password.BcryptHasher{Cost: 4},
	})

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes(customPrefix, r)

	// Routes at the default prefix must NOT respond.
	defaultRec := httptest.NewRecorder()
	r.ServeHTTP(defaultRec, httptest.NewRequest(http.MethodPost, "/auth/user"+password.PathLogin, nil))
	if defaultRec.Code != http.StatusNotFound && defaultRec.Code != http.StatusMethodNotAllowed {
		t.Errorf("default-prefix login code = %d, want 404/405 — routes leaked to the old hardcoded prefix", defaultRec.Code)
	}

	// Routes at the custom prefix must respond.
	hash, err := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := store.Create(context.Background(), "alice@example.com", hash); err != nil {
		t.Fatalf("create: %v", err)
	}

	loginReq := httptest.NewRequest(http.MethodPost, customPrefix+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, loginReq)
	if rec.Code != http.StatusNoContent {
		t.Errorf("custom-prefix login code = %d, want 204", rec.Code)
	}
}

// TestRegisterRoutes_TrailingSlashPrefix proves a trailing slash on
// the supplied prefix is normalised so the registered URL is
// /auth/user/login, not /auth/user//login.
func TestRegisterRoutes_TrailingSlashPrefix(t *testing.T) {
	store := newMemStore()
	sessMgr, err := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ph := password.New(store, sessMgr, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes("/auth/user/", r) // trailing slash

	hash, _ := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	_, _ = store.Create(context.Background(), "alice@example.com", hash)

	req := httptest.NewRequest(http.MethodPost, "/auth/user"+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("login code with trailing-slash prefix = %d, want 204 — prefix normalisation may be broken", rec.Code)
	}
}

// TestRegisterRoutes_MissingLeadingSlash proves a prefix without a
// leading slash is normalised. The package opts for silent
// correction (matching the trailing-slash behaviour) rather than
// panicking, so a typo turns into working routes at the obvious
// place instead of 404s in production.
func TestRegisterRoutes_MissingLeadingSlash(t *testing.T) {
	store := newMemStore()
	sessMgr, err := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ph := password.New(store, sessMgr, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes("auth/user", r) // NO leading slash

	hash, _ := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	_, _ = store.Create(context.Background(), "alice@example.com", hash)

	// The canonical URL must respond. If RegisterRoutes had passed
	// "auth/user/login" to the router unchanged, this request would
	// 404 — most Go routers reject patterns without a leading slash.
	req := httptest.NewRequest(http.MethodPost, "/auth/user"+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("login code = %d, want 204 — missing leading slash should be normalised, not silently broken",
			rec.Code)
	}
}

// TestRegisterRoutes_BothEndsNormalised pins that the leading-slash
// and trailing-slash normalisations compose — "auth/user/" produces
// the same canonical mount as "/auth/user".
func TestRegisterRoutes_BothEndsNormalised(t *testing.T) {
	store := newMemStore()
	sessMgr, err := session.New(session.Config[auth.Identity]{
		Store:          session.NewMemoryStore[auth.Identity](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ph := password.New(store, sessMgr, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes("auth/user/", r) // both leading missing AND trailing present

	hash, _ := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	_, _ = store.Create(context.Background(), "alice@example.com", hash)

	req := httptest.NewRequest(http.MethodPost, "/auth/user"+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("login code = %d, want 204 — combined leading-missing + trailing-present should normalise to /auth/user",
			rec.Code)
	}
}

// --- richer payload (AppSession with extra fields) ---

// appSession is the named-field form: an explicit Identity field
// plus a hand-written AuthIdentity method. Used by callers who want
// a custom layout or a specific JSON shape (the embedded form
// flattens the Identity fields to top level).
type appSession struct {
	Identity auth.Identity
	Locale   string
	CartSize int
}

func (s *appSession) AuthIdentity() *auth.Identity { return &s.Identity }

// embeddedSession is the primary, simplest user-facing form: embed
// auth.Identity by value. *embeddedSession picks up AuthIdentity via
// Go's method promotion — no method on embeddedSession itself.
type embeddedSession struct {
	auth.Identity
	Locale   string
	CartSize int
}

// TestRicherSessionPayload_PreservesNonIdentityFields proves the
// whole point of the Identifiable design: an app can carry other
// state in the same session and password's writes only touch the
// Identity slot.
func TestRicherSessionPayload_PreservesNonIdentityFields(t *testing.T) {
	store := newMemStore()
	sessMgr, err := session.New(session.Config[appSession]{
		Store:          session.NewMemoryStore[appSession](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ph := password.New(store, sessMgr, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	// Pre-seed a user.
	hash, err := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := store.Create(context.Background(), "alice@example.com", hash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes(defaultPrefix, r)

	// Add a route that stamps non-identity fields BEFORE login.
	r.Post("/seed-locale", func(w http.ResponseWriter, req *http.Request) {
		if err := sessMgr.Update(req.Context(), func(s *appSession) error {
			s.Locale = "hu"
			s.CartSize = 3
			return nil
		}); err != nil {
			t.Fatalf("seed Update: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Round 1: seed Locale and CartSize.
	seedRec := httptest.NewRecorder()
	r.ServeHTTP(seedRec, httptest.NewRequest(http.MethodPost, "/seed-locale", nil))
	cookies := seedRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("seed did not set a cookie")
	}

	// Round 2: login carrying the same cookie.
	loginReq := httptest.NewRequest(http.MethodPost, defaultPrefix+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		loginReq.AddCookie(c)
	}
	loginRec := httptest.NewRecorder()
	r.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusNoContent {
		t.Fatalf("login code = %d, want 204", loginRec.Code)
	}

	// Round 3: probe — Locale and CartSize must survive; Identity
	// must be set.
	postLoginCookies := loginRec.Result().Cookies()
	if len(postLoginCookies) == 0 {
		// Promote may have rotated the SID; fall back to merged set.
		postLoginCookies = cookies
	}
	probeReq := httptest.NewRequest(http.MethodGet, "/probe", nil)
	for _, c := range postLoginCookies {
		probeReq.AddCookie(c)
	}
	var got appSession
	probe := router.New()
	probe.Use(sessMgr.Middleware)
	probe.Get("/probe", func(w http.ResponseWriter, req *http.Request) {
		p, err := sessMgr.Get(req.Context())
		if err == nil && p != nil {
			got = *p
		}
	})
	probe.ServeHTTP(httptest.NewRecorder(), probeReq)

	if got.Identity.Subject != u.ID {
		t.Errorf("Identity.Subject = %q, want %q", got.Identity.Subject, u.ID)
	}
	if got.Locale != "hu" {
		t.Errorf("Locale = %q, want hu — password.New must not clobber non-identity fields", got.Locale)
	}
	if got.CartSize != 3 {
		t.Errorf("CartSize = %d, want 3 — password.New must not clobber non-identity fields", got.CartSize)
	}
}

// TestEmbeddedSessionPayload_EndToEnd proves the embedding pattern
// works through password.New end to end: no AuthIdentity method on
// embeddedSession itself, login writes through the promoted method,
// non-identity fields survive, the provider's Authenticate reads
// identity back from the session.
func TestEmbeddedSessionPayload_EndToEnd(t *testing.T) {
	store := newMemStore()
	sessMgr, err := session.New(session.Config[embeddedSession]{
		Store:          session.NewMemoryStore[embeddedSession](),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}

	// Compile-time check that the embedding gets us Identifiable for
	// free, without writing a method on embeddedSession.
	var _ auth.Identifiable = (*embeddedSession)(nil)

	ph := password.New(store, sessMgr, password.Options{Hasher: password.BcryptHasher{Cost: 4}})

	// Pre-seed a user.
	hash, err := password.BcryptHasher{Cost: 4}.Hash("correct-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := store.Create(context.Background(), "alice@example.com", hash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	r := router.New()
	r.Use(sessMgr.Middleware)
	ph.RegisterRoutes(defaultPrefix, r)

	// Stamp non-identity fields BEFORE login.
	r.Post("/seed-locale", func(w http.ResponseWriter, req *http.Request) {
		if err := sessMgr.Update(req.Context(), func(s *embeddedSession) error {
			s.Locale = "hu"
			s.CartSize = 5
			return nil
		}); err != nil {
			t.Fatalf("seed Update: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Round 1: seed.
	seedRec := httptest.NewRecorder()
	r.ServeHTTP(seedRec, httptest.NewRequest(http.MethodPost, "/seed-locale", nil))
	cookies := seedRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("seed did not set a cookie")
	}

	// Round 2: login.
	loginReq := httptest.NewRequest(http.MethodPost, defaultPrefix+password.PathLogin,
		strings.NewReader(`{"email":"alice@example.com","password":"correct-password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		loginReq.AddCookie(c)
	}
	loginRec := httptest.NewRecorder()
	r.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusNoContent {
		t.Fatalf("login code = %d, want 204", loginRec.Code)
	}

	postLoginCookies := loginRec.Result().Cookies()
	if len(postLoginCookies) == 0 {
		postLoginCookies = cookies
	}

	// Round 3: probe — Locale/CartSize survive, embedded Identity is
	// populated, Authenticate reads it back.
	probeReq := httptest.NewRequest(http.MethodGet, "/probe", nil)
	for _, c := range postLoginCookies {
		probeReq.AddCookie(c)
	}
	var got embeddedSession
	var authID auth.Identity
	var authErr error
	probe := router.New()
	probe.Use(sessMgr.Middleware)
	probe.Get("/probe", func(w http.ResponseWriter, req *http.Request) {
		p, err := sessMgr.Get(req.Context())
		if err == nil && p != nil {
			got = *p
		}
		authID, authErr = ph.Authenticate(req)
	})
	probe.ServeHTTP(httptest.NewRecorder(), probeReq)

	if got.Subject != u.ID {
		t.Errorf("embedded Identity.Subject = %q, want %q", got.Subject, u.ID)
	}
	if got.Locale != "hu" {
		t.Errorf("Locale = %q, want hu — embedding-pattern login clobbered a non-identity field", got.Locale)
	}
	if got.CartSize != 5 {
		t.Errorf("CartSize = %d, want 5 — embedding-pattern login clobbered a non-identity field", got.CartSize)
	}

	if authErr != nil {
		t.Fatalf("Authenticate after embedded-payload login: %v", authErr)
	}
	if authID.Subject != u.ID {
		t.Errorf("Authenticate Subject = %q, want %q", authID.Subject, u.ID)
	}
	if authID.Provider != password.ProviderName {
		t.Errorf("Authenticate Provider = %q, want %q", authID.Provider, password.ProviderName)
	}
}
