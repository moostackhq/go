# csrf

Stateless CSRF protection for Go `net/http` — a signed double-submit token, no server-side storage, no session dependency.

## Features

| Feature | What it gives you |
|---|---|
| Stateless | A signed token in a cookie + the submitted form/header. No store, no `session` coupling — just an HMAC secret. |
| Router-agnostic middleware | `Protector.Middleware` is `func(http.Handler) http.Handler`; validates unsafe methods, mints tokens on safe ones. |
| Method-based | One global mount protects every state-changing route — no per-route wiring. |
| Template-ready | Package-level `Field(r)` / `Token(r)` emit a hidden input or raw token from any handler or another package's templates. |
| Defense in depth | Signed cookie (resists cookie injection), per-render masking (BREACH), and an Origin/Referer check on HTTPS — alongside your `SameSite=Lax` cookie. |

## Install

```bash
go get github.com/moostackhq/go/csrf
```

## Usage

```go
prot, err := csrf.New(csrf.Config{
    Secret: secret,                      // >= 32 bytes, stable across restarts
    Cookie: csrf.CookieOptions{Secure: true}, // true behind HTTPS
})
if err != nil {
    log.Fatal(err)
}

r.Use(prot.Middleware) // mount once; protects all unsafe methods
```

Emit the token in a form (the result goes into your template data — `html/template` FuncMaps can't see per-request state, so pass it as data):

```go
data := pageData{CSRFField: csrf.Field(r), /* ... */}
```
```html
<form method="post" action="/login">
    {{ .CSRFField }}
    <!-- ... -->
</form>
```

For `fetch`/JSON, render the raw token and send it back as a header:

```html
<meta name="csrf-token" content="{{ .CSRFToken }}">   {{/* csrf.Token(r) */}}
```
```js
fetch("/api/x", { method: "POST", headers: { "X-CSRF-Token": token } })
```

## How it works

- The middleware keeps a **canonical token** in an `HttpOnly`, HMAC-**signed** cookie. Signing means an attacker who can set cookies still can't forge a valid one.
- Forms/JS submit a **masked** copy (random pad XOR token), different every render, so the value can't be extracted via compression side-channels (BREACH).
- On an unsafe method (POST/PUT/PATCH/DELETE) the middleware recovers the cookie token, unmasks the submitted token (header first, then the form field), and compares them in constant time — rejecting with `403` on any mismatch. Safe methods (GET/HEAD/OPTIONS/TRACE) are never rejected; they just ensure a token exists.
- It also checks the **`Origin`** header whenever the browser sends one (which is on every state-changing request), requiring it to match the host or a `TrustedOrigins` entry — so this works behind a TLS-terminating proxy without trusting any forwarded header. When `Origin` is absent it falls back to **`Referer`**, but only on a directly-TLS request (over plain HTTP the Referer is unreliable, so the check is skipped there).

## Integrating with other packages

Validation is the middleware's job and keys off the HTTP method, so a package that only *handles* a POST (e.g. an auth provider's login handler) needs **no CSRF awareness** — the middleware gatekeeps before the request arrives. A package that *renders its own forms* just calls package-level `csrf.Field(r)` with the request; it needs no reference to your `Protector`, only that the app mounted the middleware.

## Config

| Field | Default | Purpose |
|---|---|---|
| `Secret` | — (required, ≥ 32 bytes) | HMAC key authenticating the cookie. Keep it stable; rotating it invalidates outstanding tokens. |
| `Cookie.Name` | `csrf_token` | Token cookie name. `HttpOnly` is always enforced. |
| `Cookie.Secure` | `false` | Set `true` behind HTTPS. |
| `Cookie.SameSite` | `Lax` | Cookie SameSite mode. |
| `Cookie.MaxAge` | `12h` | Cookie lifetime. |
| `FieldName` | `csrf_token` | Form field checked on unsafe requests. |
| `HeaderName` | `X-CSRF-Token` | Header checked (takes precedence over the form, so JSON bodies aren't parsed). |
| `TrustedOrigins` | none | Extra origins accepted by the HTTPS Origin check. |
| `ErrorHandler` | `403` text | Response for a rejected request. |

## Status

Reference code. Single secret (key rotation not yet supported); stateless, not bound to a session — which means it does **not** prevent login-CSRF (an attacker submitting their own credentials to log a victim in). Pair it with another control if that matters for your login flow.
