# router

Lightweight Go routing library on top of net/http. Adds the ergonomic glue you'd write anyway — method shortcuts, middleware chains, prefix groups, mountable sub-handlers, customisable 404 / 405 — without replacing stdlib's [`http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) underneath.

## Features

| Feature | What it gives you |
|---|---|
| Stdlib internals | Built on `http.ServeMux` (Go 1.22+); stdlib pattern syntax (method prefixes, `{name}`, `{name...}`) applies. No custom radix tree to learn or debug. |
| Method shortcuts | `r.Get("/users/{id}", h)` and friends for the seven common verbs. `Handle(method, pattern, h)` for the generic form. |
| Two-tier middleware | Root-level `Use` is **global** — wraps the entire dispatch including 404 / 405 paths, so `StripSlashes` rewrites before routing and `CORS` preflight short-circuits before method-check. Group-level `Use` is **per-route** with snapshot inheritance into sub-groups. |
| Groups with prefix | `r.Group("/api", func(api *Router) { ... })` composes prefixes and middleware. Arbitrary nesting. |
| Mount | `r.Mount("/static/", handler)` delegates a prefix to any `http.Handler`. No prefix stripping by default — wrap with `http.StripPrefix` if you want it. |
| Customisable 404 / 405 | Per-router `NotFound(h)`: most-specific group prefix wins. Root-only `MethodNotAllowed(h)`: Allow header populated from registered methods. |
| Typed path params | `router.PathInt`, `PathInt64`, `PathFloat` on top of stdlib `r.PathValue("name")`. |
| Introspection | `r.Walk(fn)` iterates every registration in registration order. The callback receives both the wrapped chain handler (what the dispatcher runs) and the original raw handler (for `*http.FileServer`-style type introspection) — useful for `myapp routes` debug commands or CI inventories. |
| Standard middleware shape | `Middleware` is `func(http.Handler) http.Handler` — anything written for any net/http-based stack drops in without conversion. |
| Stdlib only | No third-party deps. The whole core is one file you can read in an afternoon. |
| Built-in middleware | `router/middleware` ships RequestID, Logger, Recover, Timeout, RealIP, Compress, CORS, StripSlashes — drop in or replace as needed. |

## Install

```bash
go get github.com/moostackhq/go/router@latest
```

Requires Go 1.22+ (for stdlib `ServeMux` method-aware patterns and path values).

## Quickstart

```go
package main

import (
    "fmt"
    "net/http"
    "time"

    "github.com/moostackhq/go/router"
    "github.com/moostackhq/go/router/middleware"
)

func main() {
    r := router.New()

    r.Use(
        middleware.RequestID(),
        middleware.Logger(),
        middleware.Recover(),
        middleware.Compress(), // gzips bodies ≥ 1024 bytes when client accepts gzip
        middleware.Timeout(15*time.Second),
        // CORS belongs on the root so preflight OPTIONS short-circuits
        // before the method check; see "Middleware: global vs per-route"
        // below.
        middleware.CORS(middleware.CORSOptions{AllowedOrigins: []string{"*"}}),
    )

    r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
        fmt.Fprintln(w, "hello")
    })

    r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
        id, _ := router.PathInt(req, "id")
        fmt.Fprintf(w, "user %d\n", id)
    })

    r.Group("/api", func(api *router.Router) {
        // Per-route middleware: only authenticated routes pay the cost.
        api.Use(func(next http.Handler) http.Handler {
            return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
                if req.Header.Get("Authorization") == "" {
                    http.Error(w, "unauthorized", http.StatusUnauthorized)
                    return
                }
                next.ServeHTTP(w, req)
            })
        })
        api.Get("/posts", func(w http.ResponseWriter, _ *http.Request) {
            fmt.Fprintln(w, "list posts")
        })
        api.Post("/posts", func(w http.ResponseWriter, _ *http.Request) {
            fmt.Fprintln(w, "create post")
        })
    })

    http.ListenAndServe(":8080", r)
}
```

For a complete walk-through of every capability in a single short file, see [`example/demo/main.go`](example/demo/main.go).

## Concepts

### Middleware: global vs per-route

`Use` placement determines scope:

- **On the root `Router`** (returned by `New`), middleware is **global**: it wraps the entire dispatch — pattern matching, route handlers, **and** the 404 / 405 paths. This is the layer for `Logger`, `Recover`, `RequestID`, `RealIP`, `StripSlashes` (path rewriting before matching), `CORS` (preflight that must short-circuit before method check), `Compress`, `Timeout`.

- **On a `Group`**, middleware is **per-route**: it wraps handlers registered through that group (and sub-groups via snapshot inheritance). This is the layer for auth, route-specific rate limits, or anything that should only fire on real route matches. Snapshot semantics apply: routes registered **after** the `Use` call DO see the middleware; routes registered **before** are unaffected; sub-groups created before the call don't inherit it (sub-groups created after do).

Global middleware (on the root) ignores registration order — it wraps the dispatcher itself, so it applies to every route regardless of whether the route was added before or after the `Use` call. All middleware must be in place before the first request — late additions to global middleware are silently ignored (the chain is cached on first dispatch).

### Groups

```go
r.Group("/api", func(api *Router) {
    api.Use(authMiddleware)
    api.Get("/users", listUsers)

    api.Group("/v2", func(v2 *Router) {
        v2.Get("/users", listUsersV2)  // → GET /api/v2/users
        // Inherits authMiddleware via snapshot at the time of this Group call.
    })
})
```

Snapshot semantics: an inner Group captures the outer's middleware **as of the moment Group is called**. Subsequent `outer.Use(...)` does NOT retroactively apply to already-created inner groups.

### Mount

```go
r.Mount("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("public"))))
r.Mount("/pprof/", http.HandlerFunc(pprof.Index))  // or whatever
```

The mount accepts any `http.Handler` and dispatches every request under the prefix to it. No prefix stripping — the handler sees the full URL path. For Go-style strip-and-serve, wrap with `http.StripPrefix` explicitly (as above).

The prefix is normalised to end in `/` — `Mount("/static", h)` and `Mount("/static/", h)` are equivalent. If a more-specific route shares the prefix (e.g. `r.Get("/api/health", ...)` together with `r.Mount("/api/", h)`), stdlib `ServeMux`'s longest-pattern-wins rule applies: `/api/health` hits the GET handler, everything else under `/api/` hits the mount.

Use `Mount` for third-party handlers; use `Group` for sub-trees of your own routes (you get per-method routing and middleware composition).

### NotFound / MethodNotAllowed

```go
r.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
    http.Error(w, "not found", http.StatusNotFound)
}))

r.Group("/api", func(api *Router) {
    api.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        http.Error(w, `{"error":"api not found"}`, http.StatusNotFound)
    }))
})
```

Per-group `NotFound` — most-specific prefix wins. A request to `/api/missing` dispatches to `/api`'s NotFound; `/elsewhere` falls through to the root's. Setting `NotFound` twice for the same scope panics at registration time — duplicate registration is almost always a bug.

`MethodNotAllowed` is root-only (per-group scoping is intentionally out of scope). The `Allow` header is populated with the methods actually registered for the matched path before the handler runs:

```go
r.MethodNotAllowed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    // The "Allow" response header is already set by the router;
    // the request is still fully readable for logging or method-
    // specific responses.
    slog.Warn("405", "method", r.Method, "path", r.URL.Path, "allow", w.Header().Get("Allow"))
    http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
}))
```

### Path parameters

Stdlib provides `r.PathValue("id")` returning `string`. The router adds typed wrappers:

```go
id, err := router.PathInt(r, "id")     // int
n,  err := router.PathInt64(r, "n")    // int64
f,  err := router.PathFloat(r, "f")    // float64
```

For other types or fancier parsing, call `r.PathValue("...")` directly.

## Built-in middleware

`router/middleware/`:

| Middleware | What it does |
|---|---|
| `RequestID()` | Generates / preserves `X-Request-ID`. Exposed via `middleware.GetRequestID(ctx)`. |
| `Logger()` / `LoggerWith(*slog.Logger)` | One Info-level structured record per request: method, path, status, dur, bytes, remote, request_id. |
| `Recover()` / `RecoverWith(*slog.Logger)` | Panic → 500 + Error log with stack. `http.ErrAbortHandler` is detected via `errors.Is` (so a wrapped sentinel — e.g. `fmt.Errorf("client closed: %w", http.ErrAbortHandler)` — still triggers it) and re-panicked as the **bare** sentinel so stdlib's `==`-based abort handling fires regardless of wrapping. |
| `Timeout(d)` | Attaches a deadline to the request context. `d <= 0` is a no-op. |
| `RealIP()` | Mutates `RemoteAddr` from `X-Real-IP`, leftmost `X-Forwarded-For`, or the RFC 7239 `Forwarded:` header (in that priority). Preserves the original port. Trust caveat applies — only enable behind a proxy you control. |
| `Compress()` / `CompressWith(CompressOptions{MinSize: n})` | gzips the response when the client accepts. Skips bodies below `MinSize` (default 1024 bytes) and passes handler-set `Content-Encoding` through unchanged. Uses a `sync.Pool` of `*gzip.Writer`s — pooled writers are reset to `io.Discard` before being returned so an idle entry never pins a previous request's `ResponseWriter`. Panic-safe: a handler panic mid-stream still produces a structurally valid gzip response (the stream is finalised cleanly before the panic propagates). |
| `CORS(opts)` | Full preflight handling with origin allowlist, methods, headers, credentials, max-age. |
| `StripSlashes()` | Internal path rewrite (`/users/` → `/users`) before routing. Not a 301. |

Each is a `router.Middleware` you drop into `Use`.

## Comparison with chi

| Aspect | chi | this |
|---|---|---|
| Engine | Own radix tree | Stdlib `http.ServeMux` |
| Path params | `chi.URLParam(r, "id")` | `r.PathValue("id")` (stdlib) |
| Method routing | Required | Stdlib has it since 1.22 |
| Middleware shape | `func(Handler) Handler` | Same |
| Groups | ✓ | ✓ |
| External deps | None | None |

The pitch isn't "faster" — stdlib's mux post-1.22 is competitive, and we don't try to beat it. The pitch is *smaller and uses the runtime you already know*. Anyone reading the source can follow it in an afternoon.

## Status

Alpha. Public API may change.
