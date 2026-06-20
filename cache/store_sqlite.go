package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// sqliteIdent guards the table name, which is interpolated into the
// statements (it can't be a bind placeholder). It is developer config,
// not user input, but validating it keeps the interpolation safe.
var sqliteIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SQLiteStore is a [Store] backed by a SQLite table — cross-process on a
// single host, and durable across restarts (unlike [Memory]). It uses
// only database/sql; the caller registers a driver (e.g.
// modernc.org/sqlite) and passes the *sql.DB. Each operation is a single
// atomic statement, and expired rows are filtered in SQL so they read as
// absent.
//
// The loss-safe cache contract still holds: durability here is a
// convenience (a warm cache survives a restart), never a guarantee to
// depend on.
type SQLiteStore struct {
	db    *sql.DB
	table string
	now   func() time.Time

	getQ, setQ, delQ, sweepQ string
}

type sqliteConfig struct {
	table  string
	create bool
}

// SQLiteOption configures a [SQLiteStore].
type SQLiteOption func(*sqliteConfig)

// WithTable sets the table name (default "cache_entries"). It must be a
// plain SQL identifier.
func WithTable(name string) SQLiteOption {
	return func(c *sqliteConfig) { c.table = name }
}

// WithoutSchema skips CREATE TABLE — use it when the schema is owned by a
// migration tool. The table must already exist with columns
// (key TEXT PRIMARY KEY, value BLOB, expires_at INTEGER NOT NULL).
func WithoutSchema() SQLiteOption {
	return func(c *sqliteConfig) { c.create = false }
}

// NewSQLiteStore returns a SQLite-backed store. By default it creates the
// table if absent; see [WithoutSchema] and [WithTable].
func NewSQLiteStore(ctx context.Context, db *sql.DB, opts ...SQLiteOption) (*SQLiteStore, error) {
	cfg := sqliteConfig{table: "cache_entries", create: true}
	for _, o := range opts {
		o(&cfg)
	}
	if !sqliteIdent.MatchString(cfg.table) {
		return nil, fmt.Errorf("cache: invalid table name %q", cfg.table)
	}
	s := &SQLiteStore{db: db, table: cfg.table, now: time.Now}

	if cfg.create {
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	key        TEXT    PRIMARY KEY,
	value      BLOB    NOT NULL,
	expires_at INTEGER NOT NULL
)`, s.table)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return nil, fmt.Errorf("cache: create table: %w", err)
		}
	}

	// A row counts as present only while unexpired (expires_at 0 means
	// never). Set always overwrites — a cache write replaces whatever was
	// there, expired or not.
	s.getQ = fmt.Sprintf(
		`SELECT value FROM %s WHERE key = ? AND (expires_at = 0 OR expires_at > ?)`, s.table)
	s.setQ = fmt.Sprintf(
		`INSERT INTO %s (key, value, expires_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`, s.table)
	s.delQ = fmt.Sprintf(`DELETE FROM %s WHERE key = ?`, s.table)
	s.sweepQ = fmt.Sprintf(
		`DELETE FROM %s WHERE expires_at != 0 AND expires_at <= ?`, s.table)
	return s, nil
}

// Get implements [Store]. A present row's bytes are freshly allocated by
// Scan, so the caller may retain them.
func (s *SQLiteStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, s.getQ, key, s.now().UnixNano()).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache: get: %w", err)
	}
	return v, true, nil
}

// Set implements [Store]. A ttl <= 0 stores the entry with no expiry.
func (s *SQLiteStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if _, err := s.db.ExecContext(ctx, s.setQ, key, value, expiresAt(s.now(), ttl)); err != nil {
		return fmt.Errorf("cache: set: %w", err)
	}
	return nil
}

// Delete implements [Store]. Deleting an absent key is not an error.
func (s *SQLiteStore) Delete(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, s.delQ, key); err != nil {
		return fmt.Errorf("cache: delete: %w", err)
	}
	return nil
}

// Sweep deletes expired rows and returns how many were removed. Rows
// expire lazily on access regardless; call Sweep periodically to reclaim
// keys that never recur.
func (s *SQLiteStore) Sweep(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.sweepQ, s.now().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("cache: sweep: %w", err)
	}
	return res.RowsAffected()
}

// expiresAt converts a TTL to an absolute Unix-nanos deadline; a
// non-positive TTL means "never" (0).
func expiresAt(now time.Time, ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return now.Add(ttl).UnixNano()
}
