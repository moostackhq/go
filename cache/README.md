# cache

A small, loss-safe cache for Go: a typed front (`Cache[T]`) over a byte-oriented backend (`Store`), with TTL, optional eviction, and single-flight loading.

## The contract

A cached value must always be reconstructible from its source, because the cache may drop it at any moment — TTL expiry, eviction under pressure, or a process restart. **Never store anything here that isn't recomputable.** Durable state belongs in a database; this is an optimization layer, and losing it must only cost a recompute, never correctness.

## Features

| Feature | What it gives you |
|---|---|
| Typed front | `Cache[T]` marshals values through a `Codec` (JSON by default). |
| `GetOrLoad` | Miss → load → store → return, with **single-flight**: concurrent callers for the same key share one load. |
| TTL | Per-write expiry; `ttl <= 0` means no expiry. |
| Eviction | Memory backend supports LRU by entry count. |
| Pluggable backend | One byte-oriented `Store` serves many typed caches; ships an in-process `Memory` store and a `database/sql` `SQLiteStore`. |

## Install

```bash
go get github.com/moostackhq/go/cache
```

## Usage

```go
c := cache.New[Rollup](cache.NewMemory(), cache.WithNamespace("rollups"))

r, err := c.GetOrLoad(ctx, key, 30*time.Second, func(ctx context.Context) (Rollup, error) {
    return svc.computeRollup(ctx) // runs at most once across concurrent callers
})
```

`GetOrLoad` is the headline: on a miss it runs `load`, caches the result under `ttl`, and returns it. When many goroutines ask for a cold key at once, `load` runs **once** and every caller receives its result — the stampede protection that justifies a cache as its own layer.

Plain operations are there too:

```go
v, ok, err := c.Get(ctx, key)   // ok == false is a miss (not an error)
err = c.Set(ctx, key, v, ttl)   // ttl <= 0 → no expiry
err = c.Delete(ctx, key)
```

## Best-effort semantics

The cache never denies a caller a valid value because of a cache fault:

- A failed cache **read** is treated as a miss — `GetOrLoad` still runs `load`.
- A failed cache **write** after a successful load is ignored — the loaded value is returned anyway.

Because these faults are swallowed, a *structurally* broken cache (a value type the codec can't encode, a misconfigured backend) would silently re-load on every call with no signal. `WithErrorHook` is the seam to notice it:

```go
cache.New[T](store, cache.WithErrorHook(func(op, key string, err error) {
    slog.Warn("cache fault", "op", op, "key", key, "err", err) // op is "get" or "set"
}))
```

If `load` **panics**, the panic is re-raised in the leader *and* every waiting caller — a load panic stays a panic, surfaced consistently, never silently turned into a nil result.

## Backend: `Memory`

```go
m := cache.NewMemory(
    cache.WithMaxEntries(10_000),     // LRU cap; 0 (default) = unbounded
    cache.WithJanitor(time.Minute),   // background sweep of expired entries
)
defer m.Close()                       // stops the janitor (no-op without one)
```

Expired entries are reclaimed lazily on access regardless; the janitor is optional active reclamation. The store copies bytes in and out, so callers can't corrupt cached data by mutating slices.

Single-flight is **in-process** and **per-`Cache`** — it deduplicates concurrent loads within one `Cache` instance, not across machines and not across two `Cache` values sharing a `Store`. A distributed backend would need its own cross-process locking; document that when you add one.

`WithNamespace` joins with `:` and does not escape the separator, so prefer distinct fixed namespace literals (`"rollups"`, `"sessions"`) over values that could overlap by construction.

## Backend: `SQLite`

A cross-process, restart-durable backend on a single host. It uses only `database/sql` — you register a driver and pass the `*sql.DB`, so the cache module itself adds no driver dependency:

```go
import _ "modernc.org/sqlite"

db, _ := sql.Open("sqlite", "cache.db")
store, err := cache.NewSQLiteStore(ctx, db,
    cache.WithTable("cache_entries"), // default; must be a plain identifier
    // cache.WithoutSchema(),         // skip CREATE TABLE when a migration owns it
)
c := cache.New[Rollup](store)
```

Each operation is a single atomic statement; expired rows are filtered in SQL so they read as absent. Call `store.Sweep(ctx)` periodically to reclaim expired keys that never recur (they expire lazily on access regardless). Durability is a convenience — a warm cache survives a restart — never a guarantee to depend on; the loss-safe contract still holds.

## Codec

`Cache[T]` serializes through a `Codec` (default `encoding/json`). Override for gob, msgpack, or raw bytes:

```go
cache.New[T](store, cache.WithCodec(myCodec))
```

## Status

Reference code. Ships an in-process `Memory` backend and a `database/sql` `SQLiteStore`; the `Store` interface is shaped so a Redis-style backend slots in without touching `Cache[T]`. The core package has no external runtime dependencies — the SQLite backend uses only `database/sql`, and a driver is the caller's to provide.
