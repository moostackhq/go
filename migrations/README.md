# migrations

Lightweight, opinionated SQL migration library for Go.

## Install

```bash
go get github.com/moostackhq/go/migrations
```

This package brings no SQL driver. Add your own (`modernc.org/sqlite`, `github.com/jackc/pgx/v5/stdlib`, etc.).

## Quickstart

### SQLite

```go
package main

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log"

	"github.com/moostackhq/go/migrations"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func main() {
	db, err := sql.Open("sqlite", "app.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	src, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		log.Fatal(err)
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		log.Fatal(err)
	}
}
```

### Postgres

Same shape; swap the driver and dialect:

```go
import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log"

	"github.com/moostackhq/go/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func main() {
	db, err := sql.Open("pgx", "postgres://user:pass@host/db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	src, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		log.Fatal(err)
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectPostgres, src); err != nil {
		log.Fatal(err)
	}
}
```

`Migrate` creates a `schema_migrations` bookkeeping table on first use.

## Migration files

Files in the source `fs.FS` must match `NNNN_name.sql`:

- `NNNN` — a non-negative integer version. Sort order is numeric, so `0001` and `0010` order correctly. Timestamp-style versions like `20260524143022_add_email_index.sql` are supported.
- `name` — `[A-Za-z0-9_-]+`. Free-form description; not used for ordering.
- `.sql` — required. Non-`.sql` files are ignored; non-matching `.sql` files cause an error.

The body is one or more SQL statements, executed as a single `ExecContext` call per migration.

## Behavior

**Each migration commits independently.** If migration N fails, migrations 1..N-1 stay applied, N is rolled back, and N+1..M are not run.

**Migrations are immutable.** Every migration's SHA-256 is stored in `schema_migrations.checksum` when applied. On subsequent runs the on-disk checksum is compared against the stored one; a mismatch errors before any new migration runs. To change schema, write a new migration.

**Missing migrations are an error.** If `schema_migrations` records a version that no longer exists in source, `Migrate` errors out.

### Locking

- **Postgres**: a dedicated connection holds `pg_advisory_lock` for the duration of `Migrate`, serializing concurrent runners across processes. The lock auto-releases on connection close. Each migration commits in its own transaction.
- **SQLite**: each migration runs in its own `BEGIN IMMEDIATE` transaction. The writer lock is released between migrations. There is no batch-level mutex; concurrent `Migrate` calls against the same database serialize per-migration, and the loser of a race on the same migration fails with "table already exists" or a `PRIMARY KEY` error. The end state is that every migration is applied exactly once.

If a SQLite connection has no `busy_timeout` set, `Migrate` installs a 5000ms default on its dedicated connection. A caller-set value is respected.

## Status

`Status` returns a snapshot of every migration the runner can see, sorted by `Version`:

```go
statuses, err := migrations.Status(ctx, db, src)
for _, s := range statuses {
	fmt.Printf("%d_%s\t%s\n", s.Version, s.Name, s.State)
}
```

```go
type MigrationStatus struct {
	Version      int64
	Name         string
	Checksum     string    // stored in schema_migrations; empty for StatePending
	FileChecksum string    // SHA-256 of the source file; empty for StateOrphan
	AppliedAt    time.Time // zero for StatePending
	State        State     // Pending, Applied, Drifted, Orphan
}
```
