// Package auth is a pluggable authentication library built around one
// idea: a request either has an identity or it doesn't, and the rest
// of the app shouldn't care how that identity was established.
//
// Two primitives drive composition:
//
//   - [Authenticator] is the function shape backends implement to
//     identify a request. forwardauth, bearer, and password all
//     satisfy it directly.
//   - [Chain] composes Authenticators into a single chain. First in
//     the chain to return a non-error wins; [ErrUnauthenticated]
//     falls through; any other error short-circuits.
//
// Backends that need login routes (password, oauth2) expose a
// RegisterRoutes method that adds their handlers to the user's router
// directly — no central Manager, no aggregation.
//
// The auth package itself knows nothing about sessions, cookies, or
// stores. Backends that persist identity (password, oauth2) own that
// coupling internally. Session-using backends require their session
// payload type to implement [Identifiable] so they can read and write
// the Identity field without dictating the payload's overall shape.
//
// Wire-up:
//
//	type AppSession struct {
//	    auth.Identity        // embedded — *AppSession satisfies Identifiable
//	    Cart   []CartItem
//	    Locale string
//	}
//
//	sessMgr, _ := session.New(session.Config[AppSession]{...})
//
//	ph := password.New(userStore, sessMgr, password.Options{RegisterEnabled: true})
//	fa := forwardauth.New(forwardauth.Options{UserHeader: "X-Remote-User"})
//
//	r.Use(sessMgr.Middleware)
//	r.Group("/auth/user", ph.RegisterRoutes)
//
//	chain := auth.Chain(fa, ph)
//	r.Use(auth.Optional(chain))
//	r.Group("/api", func(api *router.Router) {
//	    api.Use(auth.Required(chain))
//	    api.Get("/me", handleMe)
//	})
package auth

import (
	"errors"
	"fmt"
	"net/http"
)

// Identity is the authenticated principal for a request. It is safe
// to log, cache, or pass as a context value. The Claims map makes
// value-comparison via == invalid; use [reflect.DeepEqual] or compare
// individual fields.
type Identity struct {
	// Subject is a stable opaque user ID, unique within Provider.
	// Two providers may use the same Subject string for different
	// humans; do not compare Subjects across Providers.
	Subject string

	// Email and Name are best-effort attributes the Provider
	// surfaced. Either may be empty.
	Email string
	Name  string

	// Provider names the backend that produced this Identity:
	// "password", "forward", "bearer", "google", etc. Match against
	// known constants in your code; the auth library never
	// interprets this field.
	Provider string

	// Claims is a free-form map for provider-specific extras (OIDC
	// claims, forward-auth header values, scopes). Apps reading from
	// it must trust the Provider that put values there.
	Claims map[string]any
}

// Identifiable is the contract a session payload type must satisfy
// to be used with session-backed backends like password or oauth2.
// It exposes the Identity field inside the payload as a writable
// pointer so backends can both read (login lookup) and write (login
// success) the identity without owning the payload shape.
//
// Apps own their session payload entirely. There are three ways to
// satisfy the interface, in order of decreasing simplicity:
//
// 1. Embed [Identity] (primary form). Because *Identity has an
// AuthIdentity method defined in this package, embedding Identity by
// value gives *AppSession the method via Go's method promotion —
// no method to write:
//
//	type AppSession struct {
//	    auth.Identity        // embedded
//	    Cart   []CartItem
//	    Locale string
//	}
//
// The promoted AuthIdentity returns a pointer into the embedded
// field inside this particular value, so writes through it mutate
// the right place. JSON encoding flattens the Identity fields to
// the top level of AppSession.
//
// 2. Named field + method (fallback for custom layouts or specific
// JSON shapes):
//
//	type AppSession struct {
//	    Identity auth.Identity `json:"identity"`
//	    Cart     []CartItem
//	}
//	func (s *AppSession) AuthIdentity() *auth.Identity { return &s.Identity }
//
// 3. Identity-only sessions. For apps whose session carries nothing
// but the Identity, no wrapping type is needed: *Identity already
// implements Identifiable, so session.Manager[auth.Identity] passes
// to password.New directly.
type Identifiable interface {
	AuthIdentity() *Identity
}

// AuthIdentity makes *Identity satisfy [Identifiable]. Apps whose
// session payload is exactly an Identity (no wrapping struct) can
// pass session.Manager[auth.Identity] to backends directly.
func (id *Identity) AuthIdentity() *Identity { return id }

// Authenticator extracts an [Identity] from a request. Return
// [ErrUnauthenticated] to indicate "no identity here" — the chain
// then falls through to the next authenticator. Any other error
// short-circuits the chain so a store outage doesn't accidentally let
// a request fall through to a less-trusted authenticator.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// AuthenticatorFunc is the func adapter for [Authenticator]. Use it
// to register inline authenticators without declaring a new type.
type AuthenticatorFunc func(r *http.Request) (Identity, error)

// Authenticate implements [Authenticator].
func (f AuthenticatorFunc) Authenticate(r *http.Request) (Identity, error) { return f(r) }

// ErrUnauthenticated is the sentinel for "no identity here". Return
// it from [Authenticator.Authenticate] to fall through to the next
// authenticator in a [Chain]; the request reaches [Required]'s 401 or
// [Optional]'s anonymous pass-through only after every chained
// authenticator has fallen through.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Chain composes multiple [Authenticator]s into one. The returned
// Authenticator tries each in registration order; the first to return
// without [ErrUnauthenticated] wins, any other error short-circuits.
// An empty chain always returns [ErrUnauthenticated].
//
// Panics if any element is nil, with the offending index in the
// message — same fail-loud-at-construction discipline as [Required]
// and [Optional]. A nil entry slipping through would otherwise panic
// deep inside a request handler the first time it's exercised, which
// is much harder to diagnose.
//
// Typical use puts per-request authenticators (forward-auth header,
// bearer token) before session-reading backends so an API client with
// a token doesn't accidentally fall through to a stale browser
// session.
func Chain(as ...Authenticator) Authenticator {
	cp := make([]Authenticator, len(as))
	copy(cp, as)
	for i, a := range cp {
		if a == nil {
			panic(fmt.Sprintf("auth: Chain called with nil Authenticator at index %d", i))
		}
	}
	return AuthenticatorFunc(func(r *http.Request) (Identity, error) {
		for _, a := range cp {
			id, err := a.Authenticate(r)
			if err == nil {
				return id, nil
			}
			if !errors.Is(err, ErrUnauthenticated) {
				return Identity{}, err
			}
		}
		return Identity{}, ErrUnauthenticated
	})
}
