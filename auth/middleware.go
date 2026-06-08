package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/moostackhq/go/router"
)

// identityCtxKey is unexported so the only way to read the Identity
// from a request context is via [FromContext]. One global key is
// fine — Identity is the same type everywhere and the auth package
// owns the namespace.
type identityCtxKey struct{}

// FromContext returns the [Identity] stashed by [Required] or
// [Optional]. The second return is false when no identity is
// present — typically because Optional ran on an anonymous request.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}

// Required is middleware that runs the authenticator on each
// request. On success it stashes the Identity in the request context
// (retrievable via [FromContext]) and calls the next handler. On
// [ErrUnauthenticated] it writes 401 and stops. Any other error
// (store outage, etc.) writes 500 — a misbehaving authenticator
// should fail closed, not silently let requests through.
func Required(a Authenticator) router.Middleware {
	if a == nil {
		panic("auth: Required called with nil Authenticator")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil {
				if errors.Is(err, ErrUnauthenticated) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				http.Error(w, "authentication error", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id)))
		})
	}
}

// Optional is middleware that runs the authenticator but doesn't
// require it to succeed. On success the Identity goes into the
// request context (retrievable via [FromContext]); on
// [ErrUnauthenticated] the request continues anonymously. Any other
// error writes 500 — same fail-closed rule as [Required].
//
// Use Optional on the broad part of your router (so handlers can
// surface a logged-in name), and layer [Required] on the protected
// sub-tree (e.g. /api).
func Optional(a Authenticator) router.Middleware {
	if a == nil {
		panic("auth: Optional called with nil Authenticator")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil && !errors.Is(err, ErrUnauthenticated) {
				http.Error(w, "authentication error", http.StatusInternalServerError)
				return
			}
			if err == nil {
				r = r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id))
			}
			next.ServeHTTP(w, r)
		})
	}
}
