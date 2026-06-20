# ratelimit

A small, backend-pluggable rate limiter for Go — usable as a library or as HTTP middleware for routes and route groups.

## Features

| Feature | What it gives you |
|---|---|
| GCRA algorithm | Token-bucket behavior from a single timestamp per key, with an exact `Retry-After`. |
| Library or middleware | `Limiter.Allow(ctx, key)` for any use; `Middleware` for `net/http` (works with any router taking `func(http.Handler) http.Handler`). |
| Pluggable backend | `Store` is a key→value store with TTL + compare-and-swap. `MemoryStore` ships in-package; the algorithm lives in the core, so backends stay tiny. |
| Burst + steady rate | One `Limit` expresses both: `PerMinute(100).WithBurst(20)`. |
| Per-key isolation | One slot per client key (IP by default), independent across keys. |
| Fail-open or fail-closed | Choose availability or denial when the backend errors. |

## Install

```bash
go get github.com/moostackhq/go/ratelimit
```

## Library

```go
store := ratelimit.NewMemoryStore()
lim, err := ratelimit.New(store, ratelimit.PerMinute(100).WithBurst(20))
if err != nil {
    log.Fatal(err)
}

res, err := lim.Allow(ctx, userID)
if err != nil {
    // backend error — apply your own policy
}
if !res.Allowed {
    // res.RetryAfter tells you when to try again
}
```

`Result` carries `Allowed`, `Limit` (capacity), `Remaining`, `RetryAfter` (0 when allowed), and `ResetAfter` (until the bucket is full again). `AllowN(ctx, key, n)` charges `n` at once; an `n` larger than the capacity can never be allowed.

## Middleware

```go
lim, _ := ratelimit.New(store, ratelimit.PerSecond(5))
r.Use(ratelimit.Middleware(lim)) // whole router, or a Group, or one route
```

By default it keys on client IP, sets `RateLimit-Limit` / `RateLimit-Remaining` / `RateLimit-Reset` on every response, and on a throttled request sets `Retry-After` and returns `429`.

Different limits per group compose naturally — one `Limiter` (and middleware) per group. When several limiters share a `Store`, give each a namespace so the same client key doesn't collide:

```go
api   := ratelimit.New(store, ratelimit.PerMinute(600), ratelimit.WithNamespace("api"))
login := ratelimit.New(store, ratelimit.PerMinute(5),   ratelimit.WithNamespace("login"))
```

Options:

| Option | Effect |
|---|---|
| `WithKeyFunc(fn)` | Derive the key yourself (e.g. user ID, API key). Default: `KeyByIP`. |
| `WithFailClosed()` | Deny (503) when the limiter errors or the key can't be derived. Default: fail-open. |
| `WithLimitedHandler(h)` | Custom response for a throttled request (headers are already set). |

## Limit

```go
ratelimit.PerSecond(n)              // n / second, burst n
ratelimit.PerMinute(n)              // n / minute
ratelimit.PerHour(n)                // n / hour
ratelimit.PerMinute(100).WithBurst(20) // steady 100/min, absorb 20 at once
```

`Burst` is the bucket capacity. When zero it defaults to `Rate` (a full period's worth can be spent at once). Set `Burst: 1` to forbid bursting — requests must be spaced `Period/Rate` apart.

## Backends

Two ship in-package; both implement the same three-method `Store`, and the GCRA algorithm in the core drives them with a CAS-retry loop.

**`MemoryStore`** — in-process. Two things to know:

- **It is not shared across replicas.** Behind multiple instances, each keeps its own buckets, so the effective limit is N× the configured one. Use a shared backend for a global limit.
- **Idle keys are reclaimed lazily** on next access. For a churning keyspace (per-IP keys that never recur), start a sweeper and stop it on shutdown:

  ```go
  store := ratelimit.NewMemoryStore(ratelimit.WithCleanupInterval(time.Minute))
  defer store.Close()
  ```

**`SQLiteStore`** — cross-process on a single host. Uses only `database/sql`; you register a driver and pass the `*sql.DB`:

```go
import _ "modernc.org/sqlite"

db, _ := sql.Open("sqlite", "ratelimit.db")
store, err := ratelimit.NewSQLiteStore(ctx, db) // creates the table by default
// ratelimit.WithTable("...") / ratelimit.WithoutSchema() if a migration owns it
```

Each operation is a single atomic statement; expired rows read as absent. Call `store.Sweep(ctx)` periodically to reclaim them.

Adding another backend (Redis/Postgres, or a cache that wraps one of these) means implementing `Get`, `SetIfAbsent`, `CompareAndSwap` — no algorithm code.

## Identifying the client

`KeyByIP` uses `RemoteAddr` only. It deliberately does **not** read `X-Forwarded-For` or similar — those are client-supplied and spoofable, so trusting them lets an attacker forge their key and dodge the limit. Behind a trusted proxy, pass your own `KeyFunc` that reads the specific header your infrastructure sets.

## Status

Reference code. Single algorithm (GCRA); memory and sqlite backends. Other backends slot in behind `Store`.
