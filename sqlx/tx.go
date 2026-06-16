package sqlx

import (
	"context"
	"database/sql"
	"fmt"
)

// Tx runs fn inside a database transaction. On nil return the
// transaction commits; on non-nil return the transaction rolls back
// and the error propagates. A panic inside fn rolls back and
// re-panics.
//
//	err := sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
//	    if _, err := sqlx.Insert(ctx, tx, "users", u); err != nil {
//	        return err
//	    }
//	    return sqlx.InsertMany(ctx, tx, "audit", events)
//	})
//
// The closure receives a *sql.Tx. Every sqlx helper accepts it
// (via the [Executor] interface), so the same Insert / Query / etc.
// calls work inside or outside Tx without code changes.
func Tx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlx.Tx: begin: %w", err)
	}
	panicked := true
	defer func() {
		if panicked {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		panicked = false
		if rerr := tx.Rollback(); rerr != nil {
			return fmt.Errorf("sqlx.Tx: %w (rollback: %v)", err, rerr)
		}
		return err
	}
	panicked = false
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlx.Tx: commit: %w", err)
	}
	return nil
}
