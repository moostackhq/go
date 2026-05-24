package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"
)

// pgAdvisoryLockKey is the int64 key passed to pg_advisory_lock. Derived
// from the FNV-64a hash of a namespace string so it is unlikely to collide
// with locks taken by other tools sharing the same Postgres database.
// Changing the namespace string changes the key.
var pgAdvisoryLockKey = func() int64 {
	h := fnv.New64a()
	h.Write([]byte("moostack/migrations"))
	return int64(h.Sum64())
}()

// pgDriver holds pg_advisory_lock on a dedicated connection for the
// duration of a Migrate call, serializing concurrent Migrate calls across
// processes. Each migration commits in its own transaction on that
// connection. The advisory lock auto-releases on connection close, so a
// crash leaves no stale state.
type pgDriver struct{}

func (pgDriver) placeholder(i int) string { return fmt.Sprintf("$%d", i) }

func (pgDriver) schemaSQL() string {
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL
)`
}

func (pgDriver) openSession(ctx context.Context, db *sql.DB) (session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgAdvisoryLockKey); err != nil {
		conn.Close()
		return nil, fmt.Errorf("acquire pg advisory lock: %w", err)
	}
	return &pgSession{c: conn}, nil
}

type pgSession struct {
	c      *sql.Conn
	closed bool
}

func (s *pgSession) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, query, args...)
}

func (s *pgSession) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, query, args...)
}

func (s *pgSession) apply(ctx context.Context, m migration, insertSQL string) error {
	tx, err := s.c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, insertSQL, m.version, m.name, m.checksum, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// close releases the advisory lock and closes the underlying connection.
// Uses background ctx so a canceled caller ctx does not leak the lock.
// Idempotent.
func (s *pgSession) close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlockErr error
	if _, err := s.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pgAdvisoryLockKey); err != nil {
		unlockErr = fmt.Errorf("release pg advisory lock: %w", err)
	}
	_ = s.c.Close()
	return unlockErr
}
