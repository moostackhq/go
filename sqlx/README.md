# sqlx

A modern, generics-based wrapper around `database/sql`. Typed row scanning, struct-derived writes, scoped transactions, cross-driver error classification — without the per-call struct-tag reparse, the named-param string preprocessing, or the string-keyed scan API that the original `jmoiron/sqlx` saddled you with. Field-to-column analysis happens once per type and is cached; per-row scanning still uses `reflect.Value` (same as `database/sql` internally), but never reparses tags or rebuilds the mapping.

You still write raw SQL. The library does the mechanical parts.

## Features

| Feature | What it gives you |
|---|---|
| Generic row scanning | `Query[T]` / `QueryOne[T]` return `[]T` or `T`. Field-to-column mapping is computed once per `T` and cached; per-row scanning still uses `reflect.Value` to construct scan pointers (`database/sql` also reflects internally), but struct tags are never reparsed and field lookup is an O(1) map index. |
| Struct-derived writes | `Insert`, `InsertMany`, `Update`, `Delete` build SQL from the struct's exported fields — snake_case column names, no codegen, no DSL. |
| Field name → column name | Default is snake_case (`UserID` → `user_id`, `URL` → `url`, `HTTPRequest` → `http_request`). Override per field with `db:"name"`; `db:"-"` skips the field entirely (neither written nor scanned), like `encoding/json`. For a reserved word or mixed-case column, the tag may be a double-quoted identifier (`db:"\"Order\""`): it's emitted quoted on writes and matched unquoted on reads. Field names must be ASCII. |
| Auto-PK convention | Field whose column is `id` is the auto-increment PK: zero value → excluded from INSERT and assigned by the DB; non-zero → used as-is. |
| `*sql.DB` / `*sql.Tx` interchangeable | Every helper takes an `Executor` interface satisfied by both. The same code runs inside and outside a transaction. |
| Scoped transactions | `sqlx.Tx(ctx, db, func(tx) error)` — commit on nil, rollback on error, rollback + re-panic on panic. |
| Cross-driver error classification | `errors.Is(err, sqlx.ErrUnique)` works on any backend. Each driver sub-package registers a classifier in `init()`. |
| `time.Time` normalization | RFC3339Nano TEXT on insert, multi-format parse on scan. Works with `modernc.org/sqlite` (which otherwise can't round-trip `time.Time`). |
| `JSON[T]` columns | Wraps any Go value, marshals on insert, unmarshals on scan. Backing column is TEXT (sqlite) or JSONB (postgres). |
| Nullable columns | Use the stdlib `sql.Null[T]` directly — the library reads and writes it like any other value. |
| No runtime SQL parsing | The library never reads your query strings to substitute names. The bind values are positional, exactly as `database/sql` accepts them. |

## Install

```bash
go get github.com/moostackhq/go/sqlx
go get github.com/moostackhq/go/sqlx/sqlite       # register the SQLite classifier
```

## Quickstart

```go
package main

import (
    "context"
    "database/sql"
    "errors"
    "fmt"
    "time"

    "github.com/moostackhq/go/sqlx"
    _ "github.com/moostackhq/go/sqlx/sqlite"      // registers the classifier
    _ "modernc.org/sqlite"                         // SQL driver
)

type User struct {
    ID        int64
    Email     string
    Name      sql.Null[string]
    Metadata  sqlx.JSON[map[string]any]
    CreatedAt time.Time
}

func main() {
    db, _ := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
    ctx := context.Background()

    // Insert; id is auto-assigned because User.ID is zero.
    id, err := sqlx.Insert(ctx, db, "users", User{
        Email:     "a@x",
        Name:      sql.Null[string]{V: "Alice", Valid: true},
        Metadata:  sqlx.JSON[map[string]any]{V: map[string]any{"plan": "pro"}},
        CreatedAt: time.Now().UTC(),
    })
    if errors.Is(err, sqlx.ErrUnique) {
        fmt.Println("email already taken")
        return
    }

    // Read one.
    u, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id)
    if errors.Is(err, sqlx.ErrNotFound) { /* ... */ }

    // Read many.
    us, _ := sqlx.Query[User](ctx, db, "SELECT * FROM users ORDER BY id DESC LIMIT ?", 10)

    // Transaction. The closure takes *sql.Tx; every sqlx helper accepts it.
    err = sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
        if _, err := sqlx.Insert(ctx, tx, "users", User{Email: "b@x"}); err != nil {
            return err
        }
        return sqlx.InsertMany(ctx, tx, "audit", events)
    })

    _ = u
    _ = us
}
```

## Reading

`Query[T]` and `QueryOne[T]` are the only reading primitives. `T` must be a struct.

```go
type User struct {
    ID    int64
    Email string
}

u,  err := sqlx.QueryOne[User](ctx, db, "SELECT id, email FROM users WHERE id = ?", id)
us, err := sqlx.Query[User](ctx, db, "SELECT id, email FROM users WHERE active = ?", true)
```

The library reads the rowset's column names, matches each one against the struct's fields by snake_case (or `db:` tag override), and scans positionally. Matching is **case-insensitive** — `SELECT Email`, `SELECT email`, and an `AS UserEmail` alias all map to the same field — because unquoted SQL identifiers are case-insensitive and drivers disagree on the case they report. (Caveat: PostgreSQL treats *quoted* identifiers as case-sensitive, so two columns like `"Order"` and `"order"` that are distinct there collide under this model. A deliberate simplification.) Columns in the result that don't map to a field are silently discarded; fields not present in the result keep their zero value.

`QueryOne` returns `ErrNotFound` when the query produces no rows.

### Scalars

For counts, sums, single-value picks, define a one-field struct. The decision is deliberate: a single read path keeps the rest of the API smaller.

```go
type CountResult struct{ Count int }
n, _ := sqlx.QueryOne[CountResult](ctx, db, "SELECT COUNT(*) AS count FROM users WHERE active = ?", true)
fmt.Println(n.Count)
```

### JOINs

JOINs that select same-named columns from two tables produce a `rows.Columns()` result with duplicate names (`SELECT users.id, posts.id …` → `["id", "id"]`). The library detects this at scan time and returns an error rather than silently scanning one column twice. **Alias the colliding columns in your SQL** and add matching fields on the destination struct:

```go
type UserPost struct {
    UserID    int64  `db:"user_id"`
    UserEmail string `db:"user_email"`
    PostID    int64  `db:"post_id"`
    PostTitle string `db:"post_title"`
}

posts, _ := sqlx.Query[UserPost](ctx, db, `
    SELECT users.id    AS user_id,
           users.email AS user_email,
           posts.id    AS post_id,
           posts.title AS post_title
    FROM users JOIN posts ON posts.user_id = users.id`)
```

Flat struct + `db:` tag is the pattern, on purpose. The library does not auto-flatten nested struct types — the user has to alias columns either way, and a flat struct reads better. A field that is itself a struct (embedded **or** named) is therefore rejected with [`ErrUnsupportedFieldType`] — unless it's `time.Time` or implements a `Valuer`/`Scanner` (like `sql.Null[T]` or `JSON[T]`). This is a loud error rather than a silent zero-scan: flatten it into scalar fields, give the type a `Valuer`+`Scanner`, or skip it with `db:"-"`.

## Writing

```go
func Insert(ctx context.Context, db Executor, table string, row any) (int64, error)
func InsertMany(ctx context.Context, db Executor, table string, rows any) error
func Update(ctx context.Context, db Executor, table, where string, row any, whereArgs ...any) (int64, error)
func Delete(ctx context.Context, db Executor, table, where string, whereArgs ...any) (int64, error)
```

`Insert`, `InsertMany`, and `Update` derive their column lists from the row struct's exported fields. `Delete` doesn't need a row — pass the WHERE expression and its bind args directly.

A row struct with no exported fields has no columns to write, so those three return [`ErrNoColumns`] instead of emitting a malformed statement (an empty `SET` clause or column list).

### Insert and the auto-PK convention

```go
id, err := sqlx.Insert(ctx, db, "users", User{
    Email:     "a@x",
    CreatedAt: time.Now().UTC(),
})
// id is the auto-assigned primary key
```

The library treats the column named `id` as a special case. If the corresponding field's value is its Go zero value, the field is omitted from the INSERT column list — the database assigns the id and the library returns it via `LastInsertId`. If the field has a non-zero value, the field is included verbatim (caller-supplied UUIDs, migrations, etc.) and the return value is **the supplied id itself**, not `LastInsertId`:

```go
// Caller wants a specific id (e.g. a UUID-shaped int64 they generated).
id, _ := sqlx.Insert(ctx, db, "users", User{ID: 42, Email: "explicit@x", CreatedAt: now})
// id == 42 on every driver — including pgx where LastInsertId returns 0.
// → INSERT INTO users (id, email, created_at) VALUES (?, ?, ?)
```

When the struct's *only* field is a zero-value auto-PK, every column is omitted and `Insert` emits the portable `INSERT INTO t DEFAULT VALUES` (the database assigns everything) rather than the invalid `INSERT INTO t () VALUES ()`. `InsertMany` has no portable multi-row equivalent, so an all-defaults batch returns [`ErrNoColumns`] — supply a non-PK column or call `Insert` per row.

For the auto-assigned case on drivers without `LastInsertId` (`pgx` is the common case), the return is `0`. Use raw `QueryOne` with `RETURNING id`:

```go
type Returned struct{ ID int64 }
r, _ := sqlx.QueryOne[Returned](ctx, db,
    `INSERT INTO users (email, created_at) VALUES ($1, $2) RETURNING id`,
    "a@x", time.Now().UTC())
```

Non-integer PKs (string UUIDs, byte slices) are inserted verbatim, but the `int64` return is `0` — the caller already knows the id.

### InsertMany

```go
err := sqlx.InsertMany(ctx, db, "posts", posts)
```

One `INSERT … VALUES (…),(…),(…)` statement. Auto-PK rule applies per-batch: if every row in the batch has a zero id, the id column is excluded from the statement; if any row has a non-zero id, the column is included and zeros are written as zeros (no per-row auto-assign mixing). Empty slice is a no-op, not an error.

### Update — partial via a smaller struct

`Update` sets every field in the supplied struct. To do a partial update, define a struct that contains only the columns you want to change:

```go
type UserEmailUpdate struct {
    Email string
    Name  sql.Null[string]
}

n, err := sqlx.Update(ctx, db, "users", "id = ?",
    UserEmailUpdate{Email: "new@x", Name: sql.Null[string]{V: "Bob", Valid: true}},
    userID)
// → UPDATE users SET email = ?, name = ? WHERE id = ?
```

The WHERE expression is a raw SQL fragment with positional placeholders. The args after the struct bind to those placeholders in order.

### Delete

```go
n, err := sqlx.Delete(ctx, db, "users", "id = ?", userID)
_, err = sqlx.Delete(ctx, db, "users", "created_at < ?", cutoff)
```

No row struct. Raw WHERE and positional args.

`Update` and `Delete` return the number of rows affected. The count lets you apply your own policy without dropping to raw `database/sql` — e.g. `n == 0` after a delete-by-id means "not found". The library deliberately does **not** map that to an error itself: whether a no-op delete is a failure or an idempotent success is the caller's call.

The `where` argument of `Update` / `Delete` must be **non-empty**: passing `""` emits a dangling `WHERE` and the driver returns a cryptic syntax error. To update or delete every row on purpose, pass a constant true predicate like `"1 = 1"`.

### Identifiers vs. values (SQL injection)

SQL placeholders bind **values**, never **identifiers**, so `table` and the
column names derived from your struct are interpolated into the statement
rather than parameterized. The library treats them as trusted, developer-controlled
input and guards them defensively:

- `table` is validated as a SQL identifier (optionally schema-qualified or
  double-quoted). A non-identifier — e.g. a value smuggled in from a request —
  returns [`ErrInvalidIdentifier`] and **no query runs**.
- Derived column names are validated when the struct's field map is first built;
  an invalid `db:` tag returns [`ErrInvalidIdentifier`] from the helper rather than
  reaching the database. (Two fields resolving to the same column return
  [`ErrDuplicateColumn`].)

The `where` expression passed to `Update` / `Delete` is **not** validated — it is
an arbitrary SQL fragment by design. Keep it a constant in your code and bind
every value through a `?` placeholder + `whereArgs`. Never concatenate user input
into `table` or `where`:

```go
// WRONG — request data in an identifier / WHERE position:
sqlx.Insert(ctx, db, r.FormValue("table"), row)            // rejected: ErrInvalidIdentifier
sqlx.Delete(ctx, db, "users", "name = '"+name+"'")         // injection — don't build WHERE from input

// RIGHT — identifiers are constants, values are bound:
sqlx.Delete(ctx, db, "users", "name = ?", name)
```

## Transactions

```go
err := sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
    if _, err := sqlx.Insert(ctx, tx, "users", u); err != nil {
        return err
    }
    if err := sqlx.InsertMany(ctx, tx, "audit", events); err != nil {
        return err
    }
    return nil
})
```

- Nil return → commit.
- Non-nil return → rollback, return the closure's error wrapped with `sqlx.Tx:` context.
- Panic inside fn → rollback, then re-panic.

The closure receives a `*sql.Tx`. Every sqlx helper accepts it via the `Executor` interface — same code as outside a transaction. Nested transactions are not supported (database/sql doesn't model them; use savepoints with raw SQL if you need them).

## Type wrappers

### Nullable columns — `sql.Null[T]`

Use the standard library's `database/sql.Null[T]` directly; the library reads
and writes it like any other value, so it needs no wrapper of its own:

```go
import "database/sql"

type User struct {
    ID    int64
    Email string
    Name  sql.Null[string]   // column is NULL when !Valid
}

u := User{Email: "a@x", Name: sql.Null[string]{}} // writes NULL
v := User{Email: "b@x", Name: sql.Null[string]{V: "Bob", Valid: true}} // writes "Bob"
```

On scan, `!Valid` is the NULL case and `V` is the zero value of `T`. (Contrast
with [`JSON[T]`](#jsont), which *is* a library type because it adds marshal /
unmarshal behavior that the stdlib doesn't provide.)

### `JSON[T]`

Round-trips any Go value through a JSON-encoded column. The wrapper implements `driver.Valuer` (marshal on insert) and `sql.Scanner` (unmarshal on scan):

```go
type Event struct {
    ID       int64
    Metadata sqlx.JSON[map[string]any]
}

sqlx.Insert(ctx, db, "events", Event{
    Metadata: sqlx.JSON[map[string]any]{V: map[string]any{"plan": "pro"}},
})
```

The underlying column can be TEXT (sqlite) or JSONB (postgres) — the wrapper just produces and consumes bytes.

### `time.Time` and `*time.Time`

Use either directly. `time.Time` is normalized to RFC3339Nano TEXT on insert and parsed back (RFC3339Nano plus a few common fallbacks) on scan. The scanner's first case is `time.Time → time.Time`, so a driver that already returns native `time.Time` on scan round-trips without going through the parse fallbacks.

`*time.Time` is the nullable form: a nil pointer writes NULL, a non-nil pointer is dereferenced and formatted like a value `time.Time`. On scan, NULL produces a nil pointer; a stored value produces a freshly allocated pointer to it.

**Tested against `modernc.org/sqlite`** (TEXT columns). Drivers with native `TIMESTAMP` / `TIMESTAMPTZ` columns — pgx, lib/pq, mysql — should accept the RFC3339Nano string via the standard ISO-8601 coercion most SQL engines provide, but this is **not currently verified by the library's test suite**. If you hit a coercion failure on another driver, the workaround is to write the column explicitly (raw `db.ExecContext`) until a tested sub-package lands.

```go
type Event struct {
    ID          int64
    Title       string
    ScheduledAt *time.Time   // nullable
}

sqlx.Insert(ctx, db, "events", Event{Title: "draft"})                       // scheduled_at IS NULL
sqlx.Insert(ctx, db, "events", Event{Title: "launch", ScheduledAt: &when})  // scheduled_at = '2026-07-04T15:30:00Z'
```

If you need a different on-disk format (Unix epoch, native TIMESTAMP, etc.), use a different field type and write the column explicitly.

## Error classification

```go
var (
    ErrNotFound   = errors.New("sqlx: not found")
    ErrUnique     = errors.New("sqlx: unique constraint violation")
    ErrForeignKey = errors.New("sqlx: foreign key violation")
    ErrCheck      = errors.New("sqlx: check constraint violation")
)
```

`errors.Is(err, sqlx.ErrUnique)` works across backends. Each driver sub-package registers a classifier in `init()` that recognizes that backend's error shape and maps to the sentinel.

```go
import _ "github.com/moostackhq/go/sqlx/sqlite"   // registers SQLite classifier
```

The classifier sees the raw driver error from every operation. If it returns a sentinel, `sqlx` wraps the original with `fmt.Errorf("%w: %w", sentinel, original)` — so `errors.Is(err, sqlx.ErrUnique)` AND `errors.Is(err, originalDriverError)` both return true.

### Adding your own

```go
sqlx.RegisterClassifier(func(err error) error {
    if isMyDriverUniqueError(err) { return sqlx.ErrUnique }
    return nil
})
```

Classifiers run in registration order; the first non-nil return wins. Importing a driver sub-package handles this automatically; you only call `RegisterClassifier` directly when writing your own driver layer.

## Driver sub-packages

| Package | Provides |
|---|---|
| `sqlx/sqlite` | Classifier for SQLite. Tested against `modernc.org/sqlite`. |
| `sqlx/postgres` | Planned. Will classify pgx + lib/pq error codes. |

The main `sqlx` package has no driver dependency. Sub-packages exist so importing the main package doesn't pull in any specific driver's transitive deps.

## What's NOT in this library

By design, the following live in user code or a different library:

- **A query builder.** Use raw SQL. The library never reads your query strings.
- **Named parameters.** Positional `?`/`$N` only — what `database/sql` accepts.
- **Migrations.** Use [`migrations`](../migrations) (or anything else).
- **Connection-pool tuning.** `*sql.DB.SetMaxOpenConns` etc. stay where they are.
- **An Active Record / model layer.** No `model.Save()`, no struct decoration with persistence behaviors.
- **Prepared statement caching beyond `database/sql`'s built-in.** Long-lived prepared statements are app concerns.
- **Upsert / `ON CONFLICT` helpers.** Write the SQL — the dialect details vary too much for a portable helper to add value.
- **A scanner that reparses struct tags on every call.** `T`'s field map is built once and cached in `sync.Map`. Per-row scanning still uses `reflect.Value` (see "What reflection happens, and when"), but tags are never reparsed and field lookup is O(1).

## Operating notes

### SQLite `:memory:` and the connection pool

In-memory SQLite databases are scoped to a single connection. With `database/sql`'s default pool, a second query can run on a different (empty) database. Cap the pool to a single connection for `:memory:`:

```go
db, _ := sql.Open("sqlite", ":memory:")
db.SetMaxOpenConns(1)
```

For file-backed databases this isn't an issue.

### What reflection happens, and when

Be precise about this: the library uses reflection. The question is *what's cached* and *what runs per row*.

- **Cached, once per `T`:** field discovery, snake_case + `db:` tag resolution, duplicate-column detection, the `field name → column name` and `column name → field index` maps, the auto-PK index. Built on first sight of `T`, stored in a `sync.Map`, looked up by `reflect.Type` thereafter. The struct's tags are read exactly once for the lifetime of the process.

- **Per call (not per row):** `reflect.TypeOf(*dst)` and a map lookup for the cached field map. Constant-time.

- **Per row, per column:** `reflect.Value.Field(idx).Addr().Interface()` to build the pointer that `rows.Scan` writes into. This is `reflect.Value` access, not type analysis — no tag parsing, no string ops, no map writes. `database/sql` itself reflects internally when dispatching `Scan` to drivers, so the marginal cost of the library's per-row reflection on top of stdlib is small but nonzero.

`Insert` and friends do the same shape: one `reflect.TypeOf` + cached-map lookup per call, then a per-field `reflect.Value.Field(i).Interface()` walk to assemble bind args.

If you need to eliminate this overhead entirely, the path is `database/sql` directly with hand-written scan closures. The library buys you the type-safe surface; you pay a small per-row reflection cost for it.

### Why struct-only reads

A single read path keeps the surface small. Scanning into scalars (`int`, `string`, etc.) would need either a separate function (`QueryScalar[T]`) or a runtime branch on `T.Kind()` — both more API surface than wrapping the scalar in a one-field struct at the call site. The cost of `type CountResult struct{ Count int }` is one line. The benefit is one read primitive.

### Why snake_case (and not the field name)

Go is exported-CamelCase, SQL is snake_case. Translating between them at scan time is the only sane default; otherwise every struct grows a `db:` tag for every field. The tag exists as an escape hatch for columns that don't follow the convention, not as the default mechanism.

## Status

Alpha. Public API may change.

**Tested against:** `modernc.org/sqlite` (the integration tests in `sqlx/sqlite/`). The main `sqlx` package has driver-independent unit tests (snake_case, fieldMap building, fake-`Executor` write tests). PostgreSQL / MySQL / etc. are expected to work via standard SQL coercion of the RFC3339Nano TEXT format the library writes, but those drivers do not have their own test suite yet — when they ship as `sqlx/postgres` etc., the same conformance tests run against them and any coercion gaps surface there.
