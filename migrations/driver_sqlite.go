package migrations

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"fmt"
	"time"
)

// sqliteDriver runs each migration inside its own BEGIN IMMEDIATE
// transaction on a dedicated connection. The writer lock is released
// between migrations. There is no batch-level mutex; concurrent Migrate
// calls against the same database serialize per migration, and the loser
// of a race on the same migration fails cleanly.
type sqliteDriver struct{}

func (sqliteDriver) placeholder(int) string { return "?" }

func (sqliteDriver) schemaSQL() string {
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL
)`
}

func (sqliteDriver) openSession(ctx context.Context, db *sql.DB) (session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	// Per-migration BEGIN IMMEDIATE will return SQLITE_BUSY immediately on
	// contention unless this conn has a busy_timeout. Respect a caller-
	// configured value if present; otherwise install a sane default. PRAGMA
	// is connection-scoped, so it does not perturb the caller's pool.
	var current int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&current); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read SQLite busy_timeout: %w", err)
	}
	if current <= 0 {
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("set SQLite busy_timeout: %w", err)
		}
	}
	return &sqliteSession{c: conn}, nil
}

type sqliteSession struct {
	c      *sql.Conn
	closed bool
	// tainted is set when ROLLBACK fails after BEGIN IMMEDIATE — the
	// connection may still be inside an open transaction. close() then
	// forces the pool to discard the conn rather than reuse it.
	tainted bool
}

func (s *sqliteSession) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, query, args...)
}

func (s *sqliteSession) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, query, args...)
}

func (s *sqliteSession) apply(ctx context.Context, m migration, insertSQL string) error {
	if _, err := s.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err := s.ExecContext(ctx, m.body); err != nil {
		s.rollback()
		return err
	}
	if _, err := s.ExecContext(ctx, insertSQL, m.version, m.name, m.checksum, time.Now().UTC()); err != nil {
		s.rollback()
		return err
	}
	if _, err := s.ExecContext(ctx, "COMMIT"); err != nil {
		s.rollback()
		return err
	}
	return nil
}

// rollback best-effort undoes the current SQLite tx. Uses background ctx so
// cancellation of the caller's ctx does not leave the connection in an open
// transaction. If ROLLBACK itself fails the connection state is unknown;
// the session is marked tainted so close() discards the conn instead of
// returning it to the pool.
func (s *sqliteSession) rollback() {
	if _, err := s.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		s.tainted = true
	}
}

func (s *sqliteSession) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.tainted {
		// Signal the pool that the underlying driver conn is unusable;
		// sql.Conn.Close then drops it instead of returning it to the
		// pool, so the next caller does not inherit a half-open tx.
		_ = s.c.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
	return s.c.Close()
}
