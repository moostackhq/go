package ratelimit

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

// SQLiteStore is a [Store] backed by a SQLite table — cross-process on
// a single host (unlike [MemoryStore]). It uses only database/sql; the
// caller registers a driver (e.g. modernc.org/sqlite) and passes the
// *sql.DB. Each operation is a single atomic statement, and expired
// rows are filtered in SQL so they read as absent.
type SQLiteStore struct {
	db    *sql.DB
	table string
	now   func() time.Time

	getQ, setQ, casQ, sweepQ string
}

type sqliteConfig struct {
	table  string
	create bool
}

// SQLiteOption configures a [SQLiteStore].
type SQLiteOption func(*sqliteConfig)

// WithTable sets the table name (default "rate_limits"). It must be a
// plain SQL identifier.
func WithTable(name string) SQLiteOption {
	return func(c *sqliteConfig) { c.table = name }
}

// WithoutSchema skips CREATE TABLE — use it when the schema is owned
// by a migration tool. The table must already exist with columns
// (key TEXT PRIMARY KEY, value INTEGER, expires_at INTEGER).
func WithoutSchema() SQLiteOption {
	return func(c *sqliteConfig) { c.create = false }
}

// NewSQLiteStore returns a SQLite-backed store. By default it creates
// the table if absent; see [WithoutSchema] and [WithTable].
func NewSQLiteStore(ctx context.Context, db *sql.DB, opts ...SQLiteOption) (*SQLiteStore, error) {
	cfg := sqliteConfig{table: "rate_limits", create: true}
	for _, o := range opts {
		o(&cfg)
	}
	if !sqliteIdent.MatchString(cfg.table) {
		return nil, fmt.Errorf("ratelimit: invalid table name %q", cfg.table)
	}
	s := &SQLiteStore{db: db, table: cfg.table, now: time.Now}

	if cfg.create {
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	key        TEXT    PRIMARY KEY,
	value      INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
)`, s.table)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return nil, fmt.Errorf("ratelimit: create table: %w", err)
		}
	}

	// A row counts as present only while unexpired (expires_at 0 means
	// never). SetIfAbsent overwrites an expired row via the upsert's
	// WHERE; CAS treats an expired row as absent.
	s.getQ = fmt.Sprintf(
		`SELECT value FROM %s WHERE key = ? AND (expires_at = 0 OR expires_at > ?)`, s.table)
	s.setQ = fmt.Sprintf(
		`INSERT INTO %s (key, value, expires_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at
WHERE %s.expires_at != 0 AND %s.expires_at <= ?`, s.table, s.table, s.table)
	s.casQ = fmt.Sprintf(
		`UPDATE %s SET value = ?, expires_at = ? WHERE key = ? AND value = ? AND (expires_at = 0 OR expires_at > ?)`, s.table)
	s.sweepQ = fmt.Sprintf(
		`DELETE FROM %s WHERE expires_at != 0 AND expires_at <= ?`, s.table)
	return s, nil
}

// Get implements [Store].
func (s *SQLiteStore) Get(ctx context.Context, key string) (int64, bool, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, s.getQ, key, s.now().UnixNano()).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: get: %w", err)
	}
	return v, true, nil
}

// SetIfAbsent implements [Store].
func (s *SQLiteStore) SetIfAbsent(ctx context.Context, key string, value int64, ttl time.Duration) (bool, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, s.setQ, key, value, expiresAt(now, ttl), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: setifabsent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: setifabsent: rows: %w", err)
	}
	return n == 1, nil
}

// CompareAndSwap implements [Store].
func (s *SQLiteStore) CompareAndSwap(ctx context.Context, key string, oldValue, newValue int64, ttl time.Duration) (bool, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, s.casQ, newValue, expiresAt(now, ttl), key, oldValue, now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("ratelimit: compareandswap: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ratelimit: compareandswap: rows: %w", err)
	}
	return n == 1, nil
}

// Sweep deletes expired rows and returns how many were removed. Rows
// expire lazily on access regardless; call Sweep periodically to
// reclaim keys that never recur.
func (s *SQLiteStore) Sweep(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.sweepQ, s.now().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
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
