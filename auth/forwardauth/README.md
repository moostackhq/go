# auth/forwardauth

An `auth.Authenticator` that reads the authenticated identity from headers set by an upstream reverse proxy.

Use it behind Caddy's `forward_auth`, Traefik's `forwardauth` middleware, oauth2-proxy, or any setup where another service decides whether a request is authorised and forwards the resulting identity as headers. The auth library trusts those headers.

## Trust caveat — load-bearing

The headers this provider reads are trusted unconditionally. **Only enable it when your app is unreachable directly** — i.e. only the proxy can connect to the listening socket. A direct client can forge any header the proxy is meant to set.

The proxy MUST strip these headers off inbound requests before forwarding so a real client can't smuggle them in. Caddy:

```caddy
example.com {
    forward_auth http://auth-server {
        uri /authenticate
        # Caddy strips client-supplied X-Remote-* and re-sets them
        # from the auth response's same-named headers.
        copy_headers X-Remote-User X-Remote-Email X-Remote-Groups
    }
    reverse_proxy localhost:8080
}
```

## Install

```bash
go get github.com/moostackhq/go/auth/forwardauth@latest
```

## Usage

```go
import (
    "github.com/moostackhq/go/auth"
    "github.com/moostackhq/go/auth/forwardauth"
    "github.com/moostackhq/go/auth/password"
)

fa := forwardauth.New(forwardauth.Options{
    UserHeader:   "X-Remote-User",   // required
    EmailHeader:  "X-Remote-Email",
    NameHeader:   "X-Remote-Name",
    GroupsHeader: "X-Remote-Groups",
})

ph := password.New(userStore, sessMgr, password.Options{...})

// Add to your auth chain. Session reads happen via the password
// provider's own Authenticate method — no separate "SessionAuth"
// adapter is needed.
chain := auth.Chain(fa, ph)
r.Use(auth.Optional(chain))
```

## Options

| Field | Required | What it does |
|---|---|---|
| `UserHeader` | yes | Header carrying the authenticated subject. Empty header value → `ErrUnauthenticated`. |
| `EmailHeader` | no | Populates `Identity.Email`. |
| `NameHeader` | no | Populates `Identity.Name`. |
| `GroupsHeader` | no | Comma-separated; split into `[]string` and stored at `Identity.Claims["groups"]`. Empty entries skipped. |
| `ProviderName` | no | Override the default `"forward"` value on `Identity.Provider`. Useful when you've configured multiple distinct forward-auth tiers (e.g. `"internal"`, `"vendor"`) and want handlers to distinguish them. |

## Chain placement

Forward-auth fits naturally first in the chain — header-based credentials are more specific than a session cookie:

```go
chain := auth.Chain(
    forwardauth.New(...),  // checked first
    ph,                    // session-reading backend (e.g. *password.Provider)
                           //   — its Authenticate reads identity from the session
)
```

If `X-Remote-User` is present, the session lookup never runs. If absent, the chain falls through to the session-based path. Any backend that satisfies `auth.Authenticator` (currently `*password.Provider[T]`, future bearer / OAuth2 backends) fits the second slot.

## Status

Alpha. Public API may change.
