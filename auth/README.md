# auth

Pluggable authentication library for Go HTTP servers. One identity contract, swappable backends — forward auth (trusted proxy header), password (with shipped login + register + logout routes), bearer tokens, OAuth2 — composed through a single `Chain` function. No central Manager.

The auth core knows nothing about sessions, cookies, or stores. Backends that persist identity (password, oauth2) own that coupling internally. Apps own their session payload entirely — auth only requires a one-method interface so backends can read and write the Identity field inside whatever payload shape the app picked.

## Features

| Feature | What it gives you |
|---|---|
| Single identity contract | `auth.Identity` is the authenticated principal; handlers read it via `auth.FromContext` and never care which backend produced it. |
| Function-shaped composition | `auth.Chain(a, b, c)` composes Authenticators. First non-error wins; `ErrUnauthenticated` falls through; any other error short-circuits. No Manager, no aggregator. |
| App owns the session payload | Apps define their own session payload type carrying identity + cart + locale + whatever. Embed `auth.Identity` and the auth backends pick up identity reads/writes for free via Go's method promotion — no glue code, no adapter struct. |
| Backends own their routes | Each backend with login routes exposes `RegisterRoutes(prefix, r)` that adds handlers directly to your router under a prefix the app chooses. No Mount gymnastics, no MountableAuth interface. |
| Middleware as free functions | `auth.Required(chain)` (401 on miss) and `auth.Optional(chain)` (populate context, don't 401). Both fail closed on store errors. |
| Customizable | `OnSuccess` / `OnFailure` / `Parser` / `OnRegistered` hooks let you serve JSON, redirect, render HTMX, swap field names. SPA-friendly defaults. |
| Timing-safe by default | Unknown-email login still runs a bcrypt verify against a dummy hash so attackers can't enumerate registered emails by response time. |
| Hasher abstraction | bcrypt(cost=12) default; swap in argon2id, a test stub, or a stricter bcrypt cost via the `Hasher` interface. |
| Stdlib + router | Core depends only on `router`. Session-using backends (password) depend on `session`. forwardauth has no session dep. |

## Install

```bash
go get github.com/moostackhq/go/auth@latest
```

Requires Go 1.22+ (for the router's `ServeMux` requirements).

## Quickstart

```go
import (
    "github.com/moostackhq/go/auth"
    "github.com/moostackhq/go/auth/forwardauth"
    "github.com/moostackhq/go/auth/password"
    "github.com/moostackhq/go/router"
    "github.com/moostackhq/go/session"
)

// AppSession is your app's session payload — anything you want
// alongside the identity. Embed auth.Identity by value and
// *AppSession satisfies auth.Identifiable via method promotion —
// no method to write.
type AppSession struct {
    auth.Identity
    Cart   []CartItem
    Locale string
}

func main() {
    sessMgr, _ := session.New(session.Config[AppSession]{ /* ... */ })

    ph := password.New(userStore, sessMgr, password.Options{RegisterEnabled: true})
    fa := forwardauth.New(forwardauth.Options{UserHeader: "X-Remote-User"})

    r := router.New()
    r.Use(sessMgr.Middleware)                      // session lib's own middleware

    ph.RegisterRoutes("/auth/user", r)             // /auth/user/login, /register, /logout

    chain := auth.Chain(fa, ph)                    // ph reads identity from the session
    r.Use(auth.Optional(chain))                    // populate identity if present

    r.Group("/api", func(api *router.Router) {
        api.Use(auth.Required(chain))              // 401 on miss
        api.Get("/me", func(w http.ResponseWriter, req *http.Request) {
            id, _ := auth.FromContext(req.Context())
            json.NewEncoder(w).Encode(id)
        })
    })

    http.ListenAndServe(":8080", r)
}
```

For a complete working app — in-memory user store, session backend, every backend wired up, curl examples in the package doc — see [`example/demo/main.go`](example/demo/main.go).

## Concepts

### `Identity`

```go
type Identity struct {
    Subject  string         // opaque user ID, unique per Provider
    Email    string
    Name     string
    Provider string         // "password" | "forward" | "bearer" | "google" | ...
    Claims   map[string]any // provider-specific extras
}
```

The authenticated principal. Handlers read it via `auth.FromContext(ctx)`. The auth library never interprets `Provider` or `Claims` — those are conventions between the producing backend and your handlers.

`Subject` is opaque to everyone but the issuing provider. `"abc123"` from `password` is NOT `"abc123"` from `google`. Don't compare Subjects across providers.

### `Identifiable`

```go
type Identifiable interface {
    AuthIdentity() *Identity
}
```

Apps own their session payload. Backends that need to read or write identity use `Identifiable` to find the identity field inside the payload — without dictating what else is in there.

**Primary form: embed `auth.Identity`.** `*auth.Identity` itself implements `Identifiable` (the method is defined on it in the auth package). When `AppSession` embeds `auth.Identity` by value, `*AppSession` picks up the method via Go's method promotion and satisfies `Identifiable` automatically — no method to write:

```go
type AppSession struct {
    auth.Identity         // embedded — Identifiable for free
    Cart      []CartItem
    Locale    string
    CSRFToken string
}
```

The promoted `AuthIdentity()` on `*AppSession` returns a pointer to the embedded `Identity` field inside that particular value, so backends can both read and write through it.

**Fallback form: named field + one method.** If you need a custom layout, a specific JSON shape (the embedded form flattens identity fields to top level), or a non-top-level position, write the method by hand:

```go
type AppSession struct {
    Identity auth.Identity `json:"identity"`  // nested under "identity" key
    Cart     []CartItem
}

func (s *AppSession) AuthIdentity() *auth.Identity { return &s.Identity }
```

Pointer receiver, pointer return — backends need to *write* through it on login, so the returned pointer must alias the field in the struct (not a copy).

**Identity-only sessions.** If the session carries nothing but the identity, use `auth.Identity` directly as the payload type. No wrapping struct, no method, no embedding — `*auth.Identity` already implements `Identifiable`:

```go
sessMgr, _ := session.New(session.Config[auth.Identity]{...})
ph := password.New(userStore, sessMgr, opts)
```

### `Authenticator` and `Chain`

```go
type Authenticator interface {
    Authenticate(r *http.Request) (Identity, error)
}

func Chain(as ...Authenticator) Authenticator
```

Every backend that identifies requests is an `Authenticator`. `Chain` composes them: walks the list in registration order, returns the first non-error. `ErrUnauthenticated` falls through; any other error short-circuits.

Typical chain puts per-request authenticators (forward-auth header, bearer token) before session-reading backends — an API client with a token shouldn't accidentally fall through to a stale browser session.

### Backend route registration

Backends that need login routes expose a `RegisterRoutes(prefix string, r *router.Router)` method. The app picks the prefix; the backend appends its known path suffixes (`/login`, `/register`, `/logout`):

```go
ph.RegisterRoutes("/auth/user", r)
// Registers POST /auth/user/login, /auth/user/register, /auth/user/logout
```

No `Mount`, no sub-router, no aggregator. Routes are first-class members of your router — visible in `r.Walk`, composable with group middleware applied later.

### Stateless / API-only

Skip session-using backends:

```go
chain := auth.Chain(bearer.New(tokenStore))   // no session
r.Use(auth.Required(chain))
```

`password.New` requires a non-nil session manager — password login needs somewhere to put the result. For pure API setups, don't use the password backend.

## Built-in backends

| Package | What it does |
|---|---|
| [`auth/forwardauth`](forwardauth/README.md) | Reads identity from a trusted reverse-proxy header (`X-Remote-User` by default). No routes. |
| [`auth/password`](password/README.md) | POST `<prefix>/login`, `<prefix>/register` (optional), `<prefix>/logout` — prefix supplied at `RegisterRoutes` time. Verifies via `Hasher` (bcrypt cost 12 default). Timing-safe against unknown-email enumeration. Generic over the app's session payload type. |

## Customisation hooks

| Hook | Default | Override when |
|---|---|---|
| `password.Options.OnSuccess` | 204 No Content | server-rendered redirect, HTMX `HX-Redirect`, custom JSON body |
| `password.Options.OnFailure` | 4-way routed: 500 logout / 500 session / 400 password-too-short / 401 everything else | re-render login page with error, surface a typed JSON error, or branch on `errors.Is` against the package sentinels |
| `password.Options.Parser` | auto-detect JSON / form, fields `email` + `password` | custom field names |
| `password.Options.OnRegistered` | auto-login (writes session) | send welcome email, manual verification flow |
| `password.Options.MinPasswordLength` | 8 | stricter policy |
| `password.Options.MaxBodyBytes` | 64 KiB | tighten if your CDN already enforces a smaller cap, raise if your custom Parser needs more (custom Parsers inherit the cap automatically via `http.MaxBytesReader`) |
| `password.Options.Hasher` | bcrypt cost 12 | argon2id, different cost, test stub |
| `forwardauth.Options.{User,Email,Name,Groups}Header` | `X-Remote-User`, rest empty | match your proxy's header names |

## User ID generation

The library never generates `User.ID` — that's your `Store.Create` implementation's job. Recommended: UUIDv7 (RFC 9562). Opaque, time-ordered for DB index locality, URL-safe:

```go
import "github.com/google/uuid"

func (s *MyStore) Create(ctx context.Context, email string, passHash []byte) (password.User, error) {
    id, err := uuid.NewV7()
    if err != nil {
        return password.User{}, err
    }
    u := password.User{ID: id.String(), Email: email, PassHash: passHash}
    return u, s.db.Insert(ctx, u)
}
```

Apps can pick ULID, autoincrement int, `crypto/rand` hex, or anything else by changing `Store.Create`.

## Security notes

- **CSRF**: login and register endpoints are CSRF targets. The auth lib does NOT ship CSRF protection. Wrap login routes with a CSRF middleware of your choice.
- **Rate limiting**: login and register endpoints are credential-stuffing targets. The auth lib does NOT ship rate limiting.
- **Timing**: `password` runs a `Hasher.Verify` against a dummy hash produced at the same cost as real stored hashes (minted once in `password.New`), so unknown-email and wrong-password requests take indistinguishable wall-clock time. The default `OnFailure` collapses credential failures, email-taken, and parse/lookup errors all to the same 401 "invalid credentials" shape.
- **Header trust** (forward auth): only enable when your app is unreachable directly. The proxy MUST strip the trusted headers off inbound requests before forwarding.
- **Cookie attributes**: the session library owns `Secure`, `HttpOnly`, `SameSite`. The auth package never touches cookies directly.

## What's NOT in v1

- OAuth2 / OIDC providers — designed, ships next.
- Bearer tokens (opaque, not JWT) — designed, ships next.
- Password change / reset endpoints — v2. The `Store.SetPassword` hook is already in place if you want to wire your own change-password or admin-reset route against the same `Store` today; the package just doesn't ship the routes themselves.
- Email verification — v2.
- Account linking — v2.
- CSRF / rate-limit middleware — separate libraries.
- HTML templates — explicitly not shipping.

## Status

Alpha. Public API may change.
