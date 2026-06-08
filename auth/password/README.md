# auth/password

Authenticates users against a `Store`'s email + password-hash table and writes a session on success.

Routes registered by `RegisterRoutes(r *router.Router)` at the path suffixes the package owns. The caller picks the mount prefix via `router.Router.Group`:

```go
r.Group("/auth/user", ph.RegisterRoutes)
```

| Route (relative) | Mounted as (with prefix `/auth/user`) | Method | Purpose |
|---|---|---|---|
| `/login` | `/auth/user/login` | POST | Verify credentials → write session → `OnSuccess` |
| `/register` | `/auth/user/register` | POST | (If `RegisterEnabled`) create user → `OnRegistered` |
| `/logout` | `/auth/user/logout` | POST | Destroy session → 204 |

Path suffixes are exported as constants — `password.PathLogin`, `password.PathRegister`, `password.PathLogout` (`"/login"` etc.) — so apps build full URLs by concatenation:

```go
loginURL := "/auth/user" + password.PathLogin  // "/auth/user/login"
```

`Group` bakes the prefix into the registered patterns at the mux level — the routes' patterns ARE the URLs they answer at (visible in `r.Walk`, no dispatch-time stripping). Prefix normalisation (trailing slashes) is `Group`'s concern, not the library's.

The package does NOT render HTML. Your app owns `GET <prefix>/login`; password only handles the POSTs.

`*password.Provider[T]` also satisfies `auth.Authenticator`. Its `Authenticate(r)` reads identity from the session, so the same provider you registered routes on can go straight into an `auth.Chain` for per-request identification.

## Install

```bash
go get github.com/moostackhq/go/auth/password@latest
```

## Usage — app-defined session payload (primary form)

The common case: your app's session carries the auth identity plus other state (cart, locale, CSRF token). Embed `auth.Identity` and `*AppSession` satisfies `auth.Identifiable` via method promotion — no method to write.

```go
import (
    "github.com/moostackhq/go/auth"
    "github.com/moostackhq/go/auth/password"
    "github.com/moostackhq/go/router"
    "github.com/moostackhq/go/session"
)

type AppSession struct {
    auth.Identity            // embedded — Identifiable for free
    Cart   []CartItem
    Locale string
}

sessMgr, _ := session.New(session.Config[AppSession]{ /* ... */ })

ph := password.New(userStore, sessMgr, password.Options{
    RegisterEnabled:   true,
    MinPasswordLength: 12,
})

r := router.New()
r.Use(sessMgr.Middleware)
r.Group("/auth/user", ph.RegisterRoutes)
```

For per-auth middleware (rate limiting, CSRF, request-ID scoped to the login flow), use the `Group` callback form:

```go
r.Group("/auth/user", func(g *router.Router) {
    g.Use(rateLimit, csrf)
    ph.RegisterRoutes(g)
})
```

`password.New` is generic over the payload type. Go infers it from the session manager — call sites never write the type parameters explicitly.

Field access also reads naturally: `s.Subject`, `s.Email`, `s.Provider` are promoted from the embedded `auth.Identity`; `s.Cart` and `s.Locale` are the app's own. `s.Identity` (using the type name as the implicit field name) also works if you prefer the explicit form.

## Usage — identity-only session

If the app's session payload IS the identity (no extra fields), use `auth.Identity` as the payload type. `*auth.Identity` implements `Identifiable` directly, so no wrapping struct is needed.

```go
sessMgr, _ := session.New(session.Config[auth.Identity]{ /* ... */ })
ph := password.New(userStore, sessMgr, password.Options{...})
```

## Usage — named field with explicit method (fallback)

Embedding flattens identity fields to the top level when serialized. If you need a nested shape (e.g. `{"identity":{...},"cart":[...]}` for JSON), or a custom layout where the identity lives somewhere other than the top level, write the method by hand:

```go
type AppSession struct {
    Identity auth.Identity `json:"identity"`
    Cart     []CartItem    `json:"cart"`
}

func (s *AppSession) AuthIdentity() *auth.Identity { return &s.Identity }
```

Same `password.New` signature. Same `ph` satisfies `auth.Authenticator`.

`password.New` panics at boot if `store` or `sessMgr` is nil — both are load-bearing.

## Options

| Field | Default | What it does |
|---|---|---|
| `OnSuccess` | 204 No Content | Runs after the session is written. Server-rendered apps redirect; HTMX apps emit `HX-Redirect`. |
| `OnFailure` | Context-aware — see below | Runs on bad creds, bad body, store error, email taken, password too short, session destroy failure. |
| `Parser` | auto-detect by Content-Type, fields `email`/`password` | Custom field names via a custom parser. |
| `RegisterEnabled` | `false` | Set `true` to register the `<prefix>/register` route. |
| `OnRegistered` | auto-login (writes session, calls `OnSuccess`) | Override for "send welcome email" / "manual verification" flows. |
| `MinPasswordLength` | `8` | Rejects shorter passwords on register. Login is not subject to this — pre-existing accounts may have shorter passwords. Zero or negative falls back to 8 (the package will not let you accidentally disable enforcement). |
| `MaxBodyBytes` | `64 KiB` (`password.DefaultMaxBodyBytes`) | Cap on the request body the parser will read on login and register. The cap is applied via `http.MaxBytesReader` before the `Parser` runs, so custom `Parser` implementations inherit it. Zero or negative falls back to the default. |
| `Hasher` | bcrypt cost 12 | Swap in argon2id, a different cost, or a test stub via the `Hasher` interface. |

## Default OnFailure routing

The default `OnFailure` is context-aware. It inspects the error and produces one of four wire shapes:

| Error shape | Wire response |
|---|---|
| Wrap with `"logout:"` prefix (session destroy failed) | 500 "logout failed" |
| Wrap with `"session:"` prefix (session write failed during login or register) | 500 "session error" |
| `errors.Is(err, ErrPasswordTooShort)` | 400 "password too short" |
| Everything else — `ErrInvalidCredentials`, `ErrEmailTaken`, parse / lookup / hash wraps | 401 "invalid credentials" |

The 401 catch-all preserves the no-information-disclosure discipline: a probing client cannot distinguish "this email is registered" from "wrong password" from "your body parsed but had an empty email" by inspecting the response shape.

### Branching in a custom OnFailure

Two supported discriminators — wrap prefixes (`strings.HasPrefix` on `err.Error()`) for failure-point routing, sentinels (`errors.Is` / `errors.As`) for cause branching. Combine them in one switch so every error gets exactly one wire response, with no implicit fallthrough between blocks:

```go
OnFailure: func(w http.ResponseWriter, r *http.Request, err error) {
    msg := err.Error()
    switch {
    // Wrap prefixes — server-side failure points. Most specific first.
    case strings.HasPrefix(msg, "logout:"):
        http.Error(w, "couldn't sign you out — try again", 500)

    case strings.HasPrefix(msg, "session:"):
        http.Error(w, "session backend unhealthy", 503)

    // Typed error — recover the configured minimum for the policy
    // rejection branch. errors.As fills pse from the wrapped chain.
    case errors.Is(err, password.ErrPasswordTooShort):
        var pse *password.PasswordTooShortError
        errors.As(err, &pse)
        http.Error(w, fmt.Sprintf("min %d characters required", pse.Min), 400)

    // Distinguish ErrEmailTaken for analytics but keep the wire
    // response generic to avoid leaking registration state.
    case errors.Is(err, password.ErrEmailTaken):
        // analytics.RegistrationCollision(r)
        http.Error(w, "invalid credentials", 401)

    case errors.Is(err, password.ErrInvalidCredentials):
        http.Error(w, "invalid credentials", 401)

    default:
        http.Error(w, "auth failed", 401)
    }
}
```

The `*PasswordTooShortError` type's `Error()` returns only `"password: too short"` — it never embeds the configured minimum, so a handler that surfaces `err.Error()` directly cannot leak the policy. The minimum is available exclusively via `errors.As` into the typed error.

## Customizing the response shape

### SPA / JSON (defaults)

POST JSON, get 204 on success, 401 on failure. Read `/me` afterward to get the identity.

### Server-rendered (redirect)

```go
password.Options{
    OnSuccess: func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
        next := r.FormValue("next")
        if next == "" { next = "/" }
        http.Redirect(w, r, next, http.StatusSeeOther)
    },
    OnFailure: func(w http.ResponseWriter, r *http.Request, _ error) {
        http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
    },
}
```

### HTMX

```go
password.Options{
    OnSuccess: func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
        w.Header().Set("HX-Redirect", "/dashboard")
        w.WriteHeader(http.StatusNoContent)
    },
    OnFailure: func(w http.ResponseWriter, _ *http.Request, _ error) {
        w.WriteHeader(http.StatusUnauthorized)
        _, _ = w.Write([]byte(`<p class="error">Invalid credentials</p>`))
    },
}
```

### Custom field names

```go
password.Options{
    Parser: func(r *http.Request) (password.Credentials, error) {
        if err := r.ParseForm(); err != nil {
            return password.Credentials{}, err
        }
        return password.Credentials{
            Email:    r.PostFormValue("username"),
            Password: r.PostFormValue("passwd"),
        }, nil
    },
}
```

## Security properties

- **Timing-safe email enumeration**: a login for an unknown email still runs `Hasher.Verify` against a dummy bcrypt hash, so wall-clock response time is indistinguishable from a wrong-password login. Pinned by `TestLogin_UnknownEmail_RunsDummyHashCheck`.
- **No information leakage in errors**: for the privacy-sensitive failure modes — email exists vs not, wrong password, registration collision (`ErrEmailTaken`) — the default `OnFailure` collapses everything to the same 401 "invalid credentials" wire shape so a probing client cannot enumerate registered emails by response. Password-too-short is distinguished as 400 "password too short" because it reveals nothing about the user (only that their input failed a public-knowledge length policy), and session/logout failures surface as 500 because those signal server-side health rather than auth state. Custom `OnFailure` implementations should preserve the email-vs-no-email collapse — log the cause server-side, keep the wire response generic. If you want strict parity with the older 401-everywhere default, override OnFailure to write a single 401 regardless of cause.
- **bcrypt cost 12** by default — ~250ms per login on commodity hardware. Bump via `password.NewBcryptHasher(cost)` or by swapping in an `argon2id`-backed `Hasher`.
- **Non-identity session fields preserved**: password writes through the `AuthIdentity()` accessor, mutating only the Identity field inside the payload. Other fields (cart, locale, CSRF) survive login/register. Pinned by `TestRicherSessionPayload_PreservesNonIdentityFields`.

## Hasher interface

```go
type Hasher interface {
    Hash(plain string) ([]byte, error)
    Verify(plain string, hash []byte) bool
}
```

Default `BcryptHasher{Cost: 12}`. Use `NewBcryptHasher(cost)` for a different bcrypt cost, or implement `Hasher` against argon2id for memory-hard hashing:

```go
import "github.com/alexedwards/argon2id"

type argonHasher struct{ params *argon2id.Params }

func (h argonHasher) Hash(plain string) ([]byte, error) {
    s, err := argon2id.CreateHash(plain, h.params)
    return []byte(s), err
}

func (h argonHasher) Verify(plain string, hash []byte) bool {
    ok, _ := argon2id.ComparePasswordAndHash(plain, string(hash))
    return ok
}
```

## Store interface

```go
type Store interface {
    LookupByEmail(ctx context.Context, email string) (User, error)
    Create(ctx context.Context, email string, passHash []byte) (User, error)
    SetPassword(ctx context.Context, userID string, passHash []byte) error
}
```

Apps implement this against their database. Errors:

- `password.ErrUserNotFound` — no matching row. Provider translates to the generic auth failure on the wire.
- `password.ErrEmailTaken` — `Create` against an existing email. Same wire translation.

Other errors propagate; the provider passes them to `OnFailure`. For Store errors specifically the default writes 401; see [Default OnFailure routing](#default-onfailure-routing) for the full table covering session-write, logout, and policy-rejection paths.

**`SetPassword` is reserved for app-side use.** The password package itself never invokes it — it's part of the `Store` contract so apps can wire their own change-password and admin-reset endpoints against the same backing store, using `BcryptHasher.Hash` (or any configured `Hasher`) to produce the `passHash` argument. The shipped routes are login / register / logout only; password changes are app-territory until v2.

## Status

Alpha. Public API may change.
