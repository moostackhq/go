package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// driver encapsulates per-engine specifics: SQL placeholder syntax, the
// schema_migrations DDL, and the per-Migrate session lifecycle (lock
// acquire, per-migration apply, lock release). The core of the package is
// dialect-agnostic and routes all engine-specific work through this
// interface. Concrete implementations live in driver_<engine>.go.
type driver interface {
	// placeholder returns the SQL placeholder for the i-th positional
	// argument (1-indexed). SQLite uses "?", Postgres uses "$N".
	placeholder(i int) string

	// schemaSQL returns the CREATE TABLE IF NOT EXISTS statement for the
	// schema_migrations bookkeeping table, using engine-appropriate column
	// types (e.g. TIMESTAMPTZ on Postgres).
	schemaSQL() string

	// openSession acquires a dedicated connection and any per-engine
	// cross-process lock the driver needs for the duration of one Migrate
	// call.
	openSession(ctx context.Context, db *sql.DB) (session, error)
}

// session is the per-Migrate-call state owned by a driver. It satisfies
// querier so the core can run ad-hoc reads and writes (schemaSQL,
// loadApplied) on the locked / configured connection without touching the
// raw *sql.Conn, and it adds apply for the per-migration body + INSERT
// step plus close for teardown.
type session interface {
	querier

	// apply runs one migration's body and records the row in
	// schema_migrations atomically. insertSQL is the prepared INSERT
	// statement with the driver's placeholder syntax already baked in.
	apply(ctx context.Context, m migration, insertSQL string) error

	// close releases the per-engine lock (if any) and the underlying
	// connection. Idempotent.
	close() error
}

func driverFor(d Dialect) (driver, error) {
	switch d {
	case DialectSQLite:
		return sqliteDriver{}, nil
	case DialectPostgres:
		return pgDriver{}, nil
	}
	return nil, fmt.Errorf("unknown dialect: %s", d)
}

// placeholders returns an N-element comma-separated list of the driver's
// positional placeholders, e.g. "?, ?, ?" or "$1, $2, $3".
func placeholders(d driver, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = d.placeholder(i + 1)
	}
	return strings.Join(parts, ", ")
}
