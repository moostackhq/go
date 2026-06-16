// Package sqlx is a modern sqlx-style wrapper over database/sql.
// It provides generic typed row scanning (Query, QueryOne), struct-
// derived write helpers (Insert, InsertMany, Update, Delete),
// scoped transactions (Tx), and cross-driver error classification
// (errors.Is(err, ErrUnique)).
//
// Design choices, locked:
//
//   - Reads always produce a struct T. Even single-column scalars
//     are wrapped in a one-field struct: the access pattern stays
//     uniform.
//   - Field-to-column mapping is snake_case of the field name
//     (UserID → user_id, URL → url, HTTPRequest → http_request).
//     Underscore insertion and lowercasing are ASCII-only; a
//     non-ASCII field name is rejected (ErrInvalidIdentifier) rather
//     than silently mis-mapped. Override per field with a
//     `db:"custom_name"` tag; `db:"-"` skips the field entirely
//     (neither written nor scanned), like encoding/json.
//   - Structs are flat: a field that is itself a struct (embedded or
//     named) is rejected with [ErrUnsupportedFieldType] unless it is
//     time.Time or implements a Valuer/Scanner (like [sql.Null] or
//     [JSON]). Nested structs are not auto-flattened — give each
//     column its own scalar field.
//   - Insert uses LastInsertId; on dialects without it (postgres),
//     the user runs the statement themselves with RETURNING.
//   - Empty string / zero numeric write as ""/0. Use [database/sql.Null]
//     for nullable semantics, [JSON] for JSON columns.
//   - The Executor interface is the subset of database/sql shared by
//     *sql.DB and *sql.Tx, so every helper accepts either.
//
// Identifiers vs. values: SQL placeholders can bind values but never
// identifiers, so table and column names are interpolated into the
// statement. The write helpers therefore treat them as trusted,
// developer-controlled input and validate them as identifiers — an
// invalid table or an invalid derived column name returns
// [ErrInvalidIdentifier] from the helper. The where expression
// passed to [Update] / [Delete] is a trusted SQL fragment and is NOT
// validated: bind its values through ? placeholders and never build
// it from user input.
package sqlx

import (
	"context"
	"database/sql"
)

// Executor is the database/sql subset every helper in this package
// accepts. Both *sql.DB and *sql.Tx satisfy it, so the same code
// works inside or outside a transaction.
type Executor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
