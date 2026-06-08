// Package password authenticates users against a [Store]'s email +
// password-hash table and writes a session on success.
//
// Routes mounted by [Provider.RegisterRoutes] at the prefix the
// caller supplies (e.g. "/auth/user"):
//
//	POST <prefix>/login    — verify credentials → write session → OnSuccess
//	POST <prefix>/register — (if RegisterEnabled) create user → OnRegistered
//	POST <prefix>/logout   — destroy session → 204
//
// The path suffixes are exposed as [PathLogin], [PathRegister], and
// [PathLogout] so apps can build the full URLs by concatenation:
//
//	loginURL := "/auth/user" + password.PathLogin
//
// The package does NOT render HTML. Your app owns GET <prefix>/login;
// password only handles the POSTs.
//
// Wire-up:
//
//	type AppSession struct {
//	    auth.Identity
//	    Cart []CartItem
//	}
//
//	sessMgr, _ := session.New(session.Config[AppSession]{...})
//	ph := password.New(userStore, sessMgr, password.Options{RegisterEnabled: true})
//	ph.RegisterRoutes("/auth/user", r)
//
// The session payload type is whatever the app wants — embedding
// [auth.Identity] (or implementing [auth.Identifiable] on a named
// field) is the only contract, so apps can store cart, locale, CSRF
// tokens, etc. alongside the identity in one session.
package password

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/moostackhq/go/auth"
	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/session"
)

// Path suffixes the provider appends to the prefix passed to
// [Provider.RegisterRoutes]. Exposed so callers can build URLs
// without restating them:
//
//	loginURL := "/auth/user" + password.PathLogin // "/auth/user/login"
const (
	PathLogin    = "/login"
	PathRegister = "/register"
	PathLogout   = "/logout"
)

// ProviderName is the value placed in [auth.Identity.Provider] on
// every Identity this backend issues. Exported so apps can branch on
// it without restating the literal:
//
//	if id.Provider == password.ProviderName { ... }
const ProviderName = "password"

// Options configures a [Provider]. All fields are zero-safe; New
// fills in defaults.
type Options struct {
	// OnSuccess runs after a successful login, auto-login from
	// register, or logout. Default: 204 No Content. Server-rendered
	// apps override with a redirect; HTMX apps respond with
	// HX-Redirect.
	//
	// For logout the id parameter is the zero auth.Identity (Subject
	// == ""). Handlers that need to route logout differently from
	// login should branch on this:
	//
	//	OnSuccess: func(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	//	    if id.Subject == "" {
	//	        http.Redirect(w, r, "/goodbye", http.StatusSeeOther)
	//	        return
	//	    }
	//	    http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	//	}
	OnSuccess func(w http.ResponseWriter, r *http.Request, id auth.Identity)

	// OnFailure runs when login, register, or logout fails for any
	// reason (bad credentials, malformed body, store outage,
	// password too short, email taken, session destroy failure).
	//
	// Default: see [defaultOnFailure] for the full routing table.
	// In summary, the default produces four wire shapes:
	//
	//   - 500 "logout failed"    — for "logout:" wraps
	//   - 500 "session error"    — for "session:" wraps (session
	//     write during login or register failed)
	//   - 400 "password too short" — for [ErrPasswordTooShort]
	//   - 401 "invalid credentials" — everything else
	//
	// The 401 branch is the no-information-disclosure default:
	// credential failures, parse errors, lookup outages,
	// [ErrInvalidCredentials], and [ErrEmailTaken] all collapse to
	// the same wire shape so a probing client can't enumerate
	// registered emails by response.
	//
	// Custom handlers can branch on either the wrap prefix (via
	// strings.HasPrefix on err.Error()) or the sentinel errors
	// (via errors.Is):
	//
	//   - Wrap prefixes: "parse:", "lookup:", "hash:", "session:",
	//     "logout:" — emitted by the corresponding failure point.
	//   - Sentinels: [ErrInvalidCredentials], [ErrPasswordTooShort]
	//     (and the typed [*PasswordTooShortError] carrying Min via
	//     errors.As), [ErrEmailTaken].
	OnFailure func(w http.ResponseWriter, r *http.Request, err error)

	// Parser extracts credentials from the request body. Default:
	// [DefaultParser] (auto-detect JSON vs form-encoded, fields
	// "email" and "password").
	Parser Parser

	// RegisterEnabled mounts PathRegister. Default false.
	RegisterEnabled bool

	// OnRegistered runs after a successful Store.Create. Default:
	// auto-login (writes session, calls OnSuccess). Override for
	// "send welcome email" / "manual verification" flows.
	OnRegistered func(w http.ResponseWriter, r *http.Request, u User)

	// MinPasswordLength rejects shorter passwords at register time.
	// Default 8 when zero or negative — the package will not let
	// you accidentally disable the minimum by passing a nonsensical
	// value. Apps that genuinely want no enforcement should set
	// MinPasswordLength = 1 (which accepts any non-empty password,
	// since empty is already rejected as ErrInvalidCredentials).
	// Login is NOT subject to this check — pre-existing accounts
	// may have shorter passwords from earlier policies.
	MinPasswordLength int

	// MaxBodyBytes caps the request body the parser will read on
	// login and register. Bodies over the cap are rejected via
	// [Options.OnFailure] before any decoding work happens, so a
	// hostile client can't push gigabytes through bcrypt. Default
	// 64 KiB — far above any legitimate credentials payload.
	//
	// The cap is enforced by wrapping r.Body with [http.MaxBytesReader]
	// before [Parser] runs, so custom Parsers inherit it automatically.
	MaxBodyBytes int64

	// Hasher overrides the default bcrypt(cost=12) hasher.
	Hasher Hasher
}

// DefaultMaxBodyBytes is the request-body cap applied to login /
// register handlers when [Options.MaxBodyBytes] is zero.
const DefaultMaxBodyBytes int64 = 64 << 10

// Provider is the password backend. T is the session payload type
// the app uses; *T must implement [auth.Identifiable] (constraint
// enforced at [New]). Construct with [New] and call
// [Provider.RegisterRoutes] to add login / register / logout to a
// router.
//
// *Provider[T] satisfies [auth.Authenticator] — drop it into an
// [auth.Chain] to read identity from the session on each request.
type Provider[T any] struct {
	store    Store
	sessMgr  *session.Manager[T]
	identity func(*T) *auth.Identity
	hasher   Hasher
	parser   Parser

	// dummyHash is computed once at New via hasher.Hash("") so the
	// unknown-email path can call hasher.Verify against a hash
	// produced by the SAME hasher at the SAME cost as real stored
	// hashes. Without per-cost matching, the timing-safety claim is
	// false: a cost-4 dummy verified against a cost-12 Hasher
	// returns ~10x faster and leaks "this email is not registered"
	// to a timing attacker.
	dummyHash []byte

	onSuccess    func(w http.ResponseWriter, r *http.Request, id auth.Identity)
	onFailure    func(w http.ResponseWriter, r *http.Request, err error)
	onRegistered func(w http.ResponseWriter, r *http.Request, u User)

	registerEnabled   bool
	minPasswordLength int
	maxBodyBytes      int64
}

// New returns a configured Provider. Panics at boot when store or
// sessMgr is nil — both are load-bearing.
//
// The type parameter T is the app's session payload; PT is the
// pointer type to that payload, constrained to satisfy
// [auth.Identifiable]. Go infers both from sessMgr's concrete type,
// so callers do not write them explicitly:
//
//	ph := password.New(userStore, sessMgr, password.Options{...})
//
// Boot cost: New performs one [Hasher.Hash] call against an empty
// string to mint the per-cost timing-safety dummy used on the
// unknown-email login path. Expect ~250ms at the default bcrypt
// cost 12 and proportionally longer at higher costs or with
// memory-hard hashers (argon2id). Call New once at startup, not
// per-request.
func New[T any, PT interface {
	*T
	auth.Identifiable
}](
	store Store,
	sessMgr *session.Manager[T],
	opts Options,
) *Provider[T] {
	if store == nil {
		panic("password: New called with nil Store")
	}
	if sessMgr == nil {
		panic("password: New called with nil session.Manager — password login requires a session backend")
	}

	p := &Provider[T]{
		store:             store,
		sessMgr:           sessMgr,
		identity:          func(t *T) *auth.Identity { return PT(t).AuthIdentity() },
		hasher:            opts.Hasher,
		parser:            opts.Parser,
		registerEnabled:   opts.RegisterEnabled,
		onSuccess:         opts.OnSuccess,
		onFailure:         opts.OnFailure,
		onRegistered:      opts.OnRegistered,
		minPasswordLength: opts.MinPasswordLength,
		maxBodyBytes:      opts.MaxBodyBytes,
	}
	if p.hasher == nil {
		p.hasher = BcryptHasher{Cost: DefaultBcryptCost}
	}
	if p.parser == nil {
		p.parser = DefaultParser
	}
	if p.onSuccess == nil {
		p.onSuccess = defaultOnSuccess
	}
	if p.onFailure == nil {
		p.onFailure = defaultOnFailure
	}
	if p.onRegistered == nil {
		p.onRegistered = p.autoLogin
	}
	if p.minPasswordLength <= 0 {
		p.minPasswordLength = 8
	}
	if p.maxBodyBytes <= 0 {
		p.maxBodyBytes = DefaultMaxBodyBytes
	}

	// Compute the timing-safety dummy hash with the resolved hasher.
	// Done once at boot so per-request login latency stays bounded by
	// a single Verify, and so the dummy's cost tracks the configured
	// hasher's cost exactly — no 10x gap for an attacker to measure.
	// Fail loud at boot if the hasher can't hash; silent fallback to
	// a wrong-cost dummy would resurrect the timing leak.
	dummy, err := p.hasher.Hash("")
	if err != nil {
		panic(fmt.Sprintf("password: New could not precompute dummy hash via configured Hasher: %v", err))
	}
	p.dummyHash = dummy

	return p
}

// RegisterRoutes adds the provider's routes to r under prefix:
//
//	POST <prefix>/login
//	POST <prefix>/register  (only if Options.RegisterEnabled)
//	POST <prefix>/logout
//
// The prefix is normalised on both ends so equivalent shapes all
// resolve to the same canonical mount point:
//
//	"/auth/user"   → /auth/user/login
//	"/auth/user/"  → /auth/user/login   (trailing slash trimmed)
//	"auth/user"    → /auth/user/login   (leading slash added)
//	"auth/user/"   → /auth/user/login   (both)
//	""             → /login             (root mount)
//
// Normalising rather than panicking matches the package's existing
// trailing-slash leniency and keeps a typo from turning into silent
// 404s in production.
func (p *Provider[T]) RegisterRoutes(prefix string, r *router.Router) {
	prefix = strings.TrimRight(prefix, "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	r.Post(prefix+PathLogin, p.handleLogin)
	if p.registerEnabled {
		r.Post(prefix+PathRegister, p.handleRegister)
	}
	r.Post(prefix+PathLogout, p.handleLogout)
}

// Authenticate reads the Identity carried by the request's session.
// Returns [auth.ErrUnauthenticated] when there is no session attached
// (request bypassed sessMgr.Middleware), when the cookie is absent or
// stale, or when the stored payload has no Subject set. Any other
// store error propagates as-is.
func (p *Provider[T]) Authenticate(r *http.Request) (auth.Identity, error) {
	payload, err := p.sessMgr.Get(r.Context())
	if err != nil {
		if errors.Is(err, session.ErrNoSession) || errors.Is(err, session.ErrNotFound) {
			return auth.Identity{}, auth.ErrUnauthenticated
		}
		return auth.Identity{}, err
	}
	if payload == nil {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	id := *p.identity(payload)
	if id.Subject == "" {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	return id, nil
}

func (p *Provider[T]) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	creds, err := p.parser(r)
	if err != nil {
		p.onFailure(w, r, fmt.Errorf("parse: %w", err))
		return
	}
	if creds.Email == "" || creds.Password == "" {
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}

	u, err := p.store.LookupByEmail(r.Context(), creds.Email)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		p.onFailure(w, r, fmt.Errorf("lookup: %w", err))
		return
	}

	if errors.Is(err, ErrUserNotFound) {
		// Timing-safe: run a Verify against the per-cost dummy hash
		// computed in New, so the response time matches a real
		// failed compare. The dummy was produced by the same Hasher
		// at the same cost as real stored hashes — no detectable
		// gap a timing attacker can use to enumerate emails.
		_ = p.hasher.Verify(creds.Password, p.dummyHash)
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}

	if !p.hasher.Verify(creds.Password, u.PassHash) {
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}

	id := identityFor(u)
	if err := p.writeSession(r.Context(), id); err != nil {
		p.onFailure(w, r, fmt.Errorf("session: %w", err))
		return
	}
	p.onSuccess(w, r, id)
}

func (p *Provider[T]) handleRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	creds, err := p.parser(r)
	if err != nil {
		p.onFailure(w, r, fmt.Errorf("parse: %w", err))
		return
	}
	if creds.Email == "" || creds.Password == "" {
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}
	if len(creds.Password) < p.minPasswordLength {
		p.onFailure(w, r, &PasswordTooShortError{Min: p.minPasswordLength})
		return
	}

	hash, err := p.hasher.Hash(creds.Password)
	if err != nil {
		p.onFailure(w, r, fmt.Errorf("hash: %w", err))
		return
	}

	u, err := p.store.Create(r.Context(), creds.Email, hash)
	if err != nil {
		p.onFailure(w, r, err)
		return
	}

	p.onRegistered(w, r, u)
}

func (p *Provider[T]) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := p.sessMgr.Destroy(r.Context()); err != nil {
		p.onFailure(w, r, fmt.Errorf("logout: %w", err))
		return
	}
	// Empty Identity signals logout context to OnSuccess — see the
	// Options.OnSuccess doc for the branching pattern.
	p.onSuccess(w, r, auth.Identity{})
}

// autoLogin is the default OnRegistered behaviour: write a session
// for the new user and call OnSuccess.
func (p *Provider[T]) autoLogin(w http.ResponseWriter, r *http.Request, u User) {
	id := identityFor(u)
	if err := p.writeSession(r.Context(), id); err != nil {
		p.onFailure(w, r, fmt.Errorf("session: %w", err))
		return
	}
	p.onSuccess(w, r, id)
}

// writeSession rotates the SID via Promote and then persists id into
// the session payload via the Identifiable accessor. Promote also
// sets the session-library-level UserID so session.Manager.ListForUser
// and RevokeAllForUser work without further wiring.
//
// Ordering matters: Promote runs FIRST so that a Promote failure
// short-circuits with no payload mutation. If Update fails after
// Promote succeeded, the request-end commit will still rotate the SID
// (Promote set renew=true) but the freshly-issued SID carries no
// identity — closing the session-fixation window where an attacker
// pre-planted a known SID could end up holding a real identity.
func (p *Provider[T]) writeSession(ctx context.Context, id auth.Identity) error {
	if err := p.sessMgr.Promote(ctx, id.Subject); err != nil {
		return err
	}
	return p.sessMgr.Update(ctx, func(payload *T) error {
		*p.identity(payload) = id
		return nil
	})
}

// identityFor builds the Identity password attaches to the session
// after a successful login or auto-login.
//
// Identity.Name is intentionally NOT populated: the password [Store]
// has no Name column and this backend has no notion of a display
// name. Apps that need a display name should look it up from their
// own user table by Subject after [auth.FromContext]. If User ever
// grows a Name field, that is a separate decision — adding it here
// without updating User would silently produce empty names.
func identityFor(u User) auth.Identity {
	return auth.Identity{
		Subject:  u.ID,
		Email:    u.Email,
		Provider: ProviderName,
	}
}

// defaultOnSuccess writes 204 No Content. SPA-friendly default.
func defaultOnSuccess(w http.ResponseWriter, _ *http.Request, _ auth.Identity) {
	w.WriteHeader(http.StatusNoContent)
}

// defaultOnFailure routes the wire response based on the error
// produced by the handler. The wrap prefixes the package emits
// ("parse:", "lookup:", "session:", "logout:") and the sentinels
// ([ErrInvalidCredentials], [ErrPasswordTooShort], [ErrEmailTaken])
// are part of [Options.OnFailure]'s documented contract, so this
// default consumes them directly:
//
//   - "logout:" wrap         → 500 "logout failed"
//   - "session:" wrap        → 500 "session error" (session write
//     during login/register failed)
//   - ErrPasswordTooShort    → 400 "password too short" (policy
//     rejection, not a credential failure)
//   - everything else        → 401 "invalid credentials"
//     (parse, lookup, hash, ErrInvalidCredentials, ErrEmailTaken —
//     all collapse to a generic 401 that does not reveal which
//     field was wrong)
//
// The 401 branch preserves the no-information-disclosure discipline
// the package follows throughout: failed-login attempts never tell
// the client whether the email is registered.
func defaultOnFailure(w http.ResponseWriter, _ *http.Request, err error) {
	switch {
	case err != nil && strings.HasPrefix(err.Error(), "logout:"):
		http.Error(w, "logout failed", http.StatusInternalServerError)
	case err != nil && strings.HasPrefix(err.Error(), "session:"):
		http.Error(w, "session error", http.StatusInternalServerError)
	case errors.Is(err, ErrPasswordTooShort):
		http.Error(w, "password too short", http.StatusBadRequest)
	default:
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}
}
