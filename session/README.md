# session

Typed HTTP sessions for Go with optimistic concurrency, identity-aware operations, and pluggable stores.

## Features

| Feature | What it gives you |
|---|---|
| Typed payload | `Manager[T any]` carries one struct. No string keys, no type asserts. |
| Lazy load, dirty tracking | The store is only read if the handler reads, only written if the handler writes. |
| Optimistic concurrency | Built-in CAS via a Version column; `Update(ctx, fn)` retries on conflict. |
| Two expiry axes | `AbsoluteExpiry` (hard, never extends) and `IdleExpiry` (sliding) tracked separately. |
| Identity-aware | `Promote` (set userID + rotate SID atomically), `ListForUser`, `RevokeAllForUser`. |
| Capability-driven stores | Stores declare what they support; missing operations error explicitly instead of silently emulating. |
| Debounced TTL bumps | Sliding-expiry refresh on read-only requests bypasses the CAS path and is throttled. |
| Multi-transport | Cookie, `Authorization: Bearer`, or both via `Multi(...)`. |

## Install

```bash
go get github.com/moostackhq/go/session
```

## Quickstart

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/moostackhq/go/session"
	_ "modernc.org/sqlite"
)

// Session is the per-user payload. Identity (userID) is NOT
// embedded here — it lives on the session record itself, set by
// mgr.Promote and read by mgr.UserID. Duplicating it in the
// payload would let the two drift apart on the next Promote call.
type Session struct {
	Cart []string `json:"cart,omitempty"`
}

func main() {
	db, err := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	store, err := session.NewSQLiteStore[Session](db, session.JSONCodec[Session]{}, session.SQLiteOptions{
		AutoCreate: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	mgr, err := session.New[Session](session.Config[Session]{
		Store:            store,
		Token:            session.Cookie{Name: "sid", HttpOnly: true, SameSite: http.SameSiteLaxMode},
		AbsoluteExpiry:   24 * time.Hour,
		IdleExpiry:       30 * time.Minute,
		IdleBumpInterval: 5 * time.Minute,
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/cart/add", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Update(r.Context(), func(s *Session) error {
			s.Cart = append(s.Cart, r.URL.Query().Get("item"))
			return nil
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		// Promote atomically rotates the SID — closes the
		// fixation hole around privilege change.
		if err := mgr.Promote(r.Context(), r.URL.Query().Get("user")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/cart", func(w http.ResponseWriter, r *http.Request) {
		s, err := mgr.Get(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		userID, _ := mgr.UserID(r.Context())
		fmt.Fprintf(w, "user=%q items=%d\n", userID, len(s.Cart))
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		_ = mgr.Destroy(r.Context())
	})

	log.Fatal(http.ListenAndServe(":8080", mgr.Wrap(mux)))
}

// Periodically remove expired rows from the SQLite store. Reads
// already filter expired sessions, so missing a sweep never returns
// stale data; the only cost is on-disk size.
func sweep(ctx context.Context, store *session.SQLiteStore[Session]) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := store.Sweep(ctx); err != nil {
				log.Printf("session sweep: %v", err)
			}
		}
	}
}
```

## Handler API

| Call | Effect |
|---|---|
| `mgr.Get(ctx)` | Lazy-loads on first call. Returns `*T` — mutations are visible to later code in the same request but are not persisted unless you also call `Save` or `Update`. |
| `mgr.UserID(ctx)` | Returns the identity attached to the session (set by `Promote`). Do not embed userID in `T` — Promote updates the record, not the payload. |
| `mgr.SID(ctx)` | Returns the current session ID. Useful as the `except` argument to `RevokeAllForUser` so the calling device stays logged in. |
| `mgr.Save(ctx)` | Marks the session dirty. The actual store write happens once, at commit, just before the response is sent. |
| `mgr.Update(ctx, fn)` | Loads, applies `fn`, writes back with CAS. Retries the closure on `ErrVersionConflict` up to `MaxRetries`. The closure must be free of external side effects — it may run more than once. |
| `mgr.Renew(ctx)` | Rotates the SID at commit. Payload is preserved; `AbsoluteExpiry` is preserved (renewals never extend the hard deadline). |
| `mgr.Promote(ctx, userID)` | Renew + attach `userID` atomically. Use on every privilege change to foreclose session-fixation. |
| `mgr.Destroy(ctx)` | Deletes the session row and clears the cookie at commit. |
| `mgr.ListForUser(ctx, userID)` | Returns SIDs of every live session for `userID`. Requires the store to implement `UserIndexer`. |
| `mgr.RevokeAllForUser(ctx, userID, except...)` | Deletes every session for `userID` except the ones listed. Typical pattern: pass `mgr.SID(ctx)` so the calling device stays logged in. |

## Stores

| Store | Build with | Optional interfaces implemented |
|---|---|---|
| Memory | `session.NewMemoryStore[T]()` | `TTLBumper` · `UserIndexer` · `Scanner` · `Sweeper` |
| SQLite | `session.NewSQLiteStore[T](db, codec, opts)` | `TTLBumper` · `UserIndexer` · `Scanner` · `Sweeper` |

Stores opt in to capabilities by implementing the matching optional interface. The manager dispatches via type assertion; calling a method against a store that does not implement the required interface returns an error wrapping `ErrCapabilityMissing`.

### SQLite schema

`SQLiteOptions.AutoCreate: true` runs the DDL on construction. To manage the schema yourself (migrations, one-off bootstrap script, ops review) use:

```go
ddl := session.SQLiteSchema()
```

The DDL is idempotent — every statement uses `IF NOT EXISTS` — so applying it on every process start is safe even when you also manage schema externally.

## Tokens

| Token | Reads | Writes | Use it for |
|---|---|---|---|
| `Cookie{Name: "sid", ...}` | The named cookie | `Set-Cookie` | Browser apps. Set `Secure`, `HttpOnly`, `SameSite` for real deploys. |
| `Bearer{}` | `Authorization: Bearer <sid>` (case-insensitive scheme) | `X-Session-Token: <sid>` | API, mobile, SPA. Read header, scheme, and write header are all configurable. |
| `Multi{Cookie{...}, Bearer{}}` | First member that has a value (in order) | Fans out to every member | Hybrid SPA+SSR. Accepts either transport; emits both on commit. |

Cookie expiry tracks `min(AbsoluteExpiry, IdleExpiry)` and is re-emitted on every touched request, so a sliding session's cookie never expires client-side before the server does.

## Configuration

```go
type Config[T any] struct {
    Store            Store[T]      // required
    Token            Token         // required
    AbsoluteExpiry   time.Duration // required; hard deadline, never extends
    IdleExpiry       time.Duration // required; sliding, must be ≤ AbsoluteExpiry
    IdleBumpInterval time.Duration // optional; debounce gap for sliding-expiry refresh, must be ≤ IdleExpiry
    MaxRetries       int           // optional; CAS retry cap for Update (default 3)
    Now              func() time.Time           // optional; test seam, defaults to time.Now
    NewSID           func() (string, error)     // optional; test seam, defaults to 32-byte crypto/rand base64url
}
```

`Now` and `NewSID` are test seams. Leave them nil in production.

## Operating notes

### SQLite under contention

Open the database with a non-zero busy timeout so concurrent writers serialize instead of failing fast:

```go
sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
```

The store does not retry `SQLITE_BUSY` itself — that's a connection-string concern. SQLite also has no row-level locks; `SQLiteStore` deliberately does not advertise the `Lock` capability.

### Sweeping expired rows

Reads filter expired rows transparently, so a missed sweep never returns stale data. The cost of skipping sweeps is on-disk size. Call `Sweep(ctx)` from a scheduler at roughly the cadence of `IdleExpiry`:

```go
n, err := store.Sweep(ctx)
```

The library does not start background goroutines for you.

### Decode failures on schema change

When the payload struct changes shape in a way that breaks JSON decoding for already-stored rows, `SQLiteStore.Load` returns `ErrNotFound` by default — the manager treats the session as expired and the user is silently re-issued a fresh one. For loud failure instead:

```go
session.SQLiteOptions{StrictDecode: true}
```

Pick strict mode when sessions carry data that can't be trivially regenerated; pick the default for browser sessions where re-login is acceptable.

### Concurrency

`Update(ctx, fn)` is the primitive for read-modify-write. The closure may run more than once (CAS retry), so it must not have external side effects — sending email, charging cards, etc. For everything else (read, then later decide to write), use `Get` and `Save`.

## Status

Alpha. Public API may change.
