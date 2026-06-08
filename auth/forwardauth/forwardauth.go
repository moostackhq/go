// Package forwardauth is an [auth.Authenticator] that reads the
// authenticated identity from headers set by a reverse proxy.
//
// Use it behind Caddy's forward_auth, Traefik's forwardauth,
// oauth2-proxy, or any setup where an upstream service decides
// whether a request is authorised and forwards the resulting
// identity as headers. The auth lib trusts those headers.
//
// Trust caveat (load-bearing): only enable this when the proxy
// owns the listening socket and your app is unreachable directly.
// A direct client can forge any header the proxy is meant to set.
// The proxy MUST strip these headers from inbound requests before
// forwarding so a real client can't smuggle them in.
//
//	# Caddy example: strip then re-set via forward_auth response.
//	forward_auth http://auth-server {
//	    uri /authenticate
//	    copy_headers X-Remote-User X-Remote-Email
//	}
package forwardauth

import (
	"net/http"
	"strings"

	"github.com/moostackhq/go/auth"
)

// Options configures a forwardauth [Authenticator].
type Options struct {
	// UserHeader is the header carrying the authenticated subject.
	// Required; New panics on empty.
	UserHeader string

	// EmailHeader, if set, populates [auth.Identity.Email] from the
	// named header on each request.
	EmailHeader string

	// NameHeader, if set, populates [auth.Identity.Name].
	NameHeader string

	// GroupsHeader, if set, is split on "," (trimming whitespace
	// around each entry) and the resulting slice is stored in
	// Identity.Claims["groups"]. Empty entries are skipped.
	GroupsHeader string

	// ProviderName overrides the default "forward" value placed in
	// [auth.Identity.Provider]. Apps surfacing multiple distinct
	// forward-auth tiers (e.g. "internal", "vendor") set this to
	// distinguish them.
	ProviderName string
}

// Authenticator is the forwardauth implementation of [auth.Authenticator].
type Authenticator struct {
	user, email, name, groups, provider string
}

// New returns a forwardauth Authenticator. Panics if opts.UserHeader
// is empty — a forwardauth backend with no user header is a
// configuration bug, surfaced loud at boot rather than as silent
// 401s in production.
func New(opts Options) *Authenticator {
	if opts.UserHeader == "" {
		panic("forwardauth: Options.UserHeader is required")
	}
	provider := opts.ProviderName
	if provider == "" {
		provider = "forward"
	}
	return &Authenticator{
		user:     opts.UserHeader,
		email:    opts.EmailHeader,
		name:     opts.NameHeader,
		groups:   opts.GroupsHeader,
		provider: provider,
	}
}

// Authenticate reads the configured headers off r. When UserHeader
// is absent or empty (after whitespace trim), returns
// [auth.ErrUnauthenticated] so [auth.Chain] falls through to the
// next backend.
func (a *Authenticator) Authenticate(r *http.Request) (auth.Identity, error) {
	subject := strings.TrimSpace(r.Header.Get(a.user))
	if subject == "" {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	id := auth.Identity{
		Subject:  subject,
		Provider: a.provider,
	}
	if a.email != "" {
		id.Email = strings.TrimSpace(r.Header.Get(a.email))
	}
	if a.name != "" {
		id.Name = strings.TrimSpace(r.Header.Get(a.name))
	}
	if a.groups != "" {
		if raw := r.Header.Get(a.groups); raw != "" {
			parts := strings.Split(raw, ",")
			kept := parts[:0]
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					kept = append(kept, p)
				}
			}
			if len(kept) > 0 {
				id.Claims = map[string]any{"groups": kept}
			}
		}
	}
	return id, nil
}
