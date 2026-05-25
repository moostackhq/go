package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// sqliteSchema is the DDL the [SQLiteStore] expects. It is idempotent
// — every statement uses IF NOT EXISTS — so it can be applied on
// every process start without harm.
const sqliteSchema = `CREATE TABLE IF NOT EXISTS sessions (
    sid             TEXT    PRIMARY KEY,
    version         INTEGER NOT NULL,
    user_id         TEXT    NOT NULL DEFAULT '',
    absolute_expiry INTEGER NOT NULL,
    idle_expiry     INTEGER NOT NULL,
    payload         BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id) WHERE user_id <> '';
CREATE INDEX IF NOT EXISTS sessions_idle_expiry ON sessions(idle_expiry);`

// SQLiteSchema returns the DDL required to back a [SQLiteStore]. Use
// it directly when you want to manage the schema yourself — committing
// it to a migrations directory, applying it through the sister
// `migrations` package, or printing it to stdout for an operator to
// run — instead of relying on [SQLiteOptions.AutoCreate].
//
// The DDL is idempotent. Calling it multiple times is a no-op.
func SQLiteSchema() string {
	return sqliteSchema
}

// SQLiteOptions configures a [SQLiteStore].
type SQLiteOptions struct {
	// AutoCreate runs [SQLiteSchema] against the database during
	// [NewSQLiteStore]. Useful for dev and tests, and for apps that
	// want the store to be self-bootstrapping. Set to false when
	// you manage schema with a migration tool or have any other
	// reason to want the side effect to be explicit.
	AutoCreate bool

	// StrictDecode controls what Load does when the codec rejects a
	// stored payload (typically because the payload struct has
	// changed shape in an incompatible way since the row was
	// written — a renamed field, a changed type).
	//
	// Default (false): graceful. Load returns [ErrNotFound], the
	// manager treats the session as gone, and the user is silently
	// re-issued a fresh session. This is the right default for
	// browser sessions where re-login is acceptable; it converts
	// every schema-change incident into "the user got logged out"
	// instead of "every request 500s."
	//
	// Set to true to propagate decode errors verbatim. Use when you
	// would rather fail loudly and investigate than silently re-auth
	// users — for example, when sessions carry data that cannot be
	// trivially regenerated.
	StrictDecode bool

	// Now is the wall-clock source the store uses for expiry
	// filtering and sweep. Tests inject a deterministic clock;
	// production leaves it nil to use time.Now.
	Now func() time.Time
}

// SQLiteStore is a [Store] backed by a SQLite database accessed
// through database/sql. It supports CAS via a version column,
// idle-expiry bumping without rewriting payload, and a secondary
// index on user_id for [UserIndexer] operations.
//
// The store does not retry SQLITE_BUSY internally — open the
// underlying *sql.DB with a sensible busy_timeout pragma (e.g.
// file:db.sqlite?_pragma=busy_timeout(5000)) to absorb contention.
//
// The store does not start a janitor. Call [SQLiteStore.Sweep]
// from a scheduler (cron, ticker, a recurring background job) on
// roughly the cadence of Config.IdleExpiry — sweeping more often
// is harmless but pointless, and sweeping much less often lets
// expired rows accumulate. Even without a sweep, expired rows are
// invisible to reads; the cost is only on-disk size.
type SQLiteStore[T any] struct {
	db           *sql.DB
	codec        Codec[T]
	now          func() time.Time
	strictDecode bool
}

// NewSQLiteStore wraps db with a session [Store]. The codec is used
// to translate session payloads to and from the payload BLOB column;
// every store has its own codec since serialization is the store's
// concern. When opts.AutoCreate is true the constructor runs
// [SQLiteSchema] against db before returning.
func NewSQLiteStore[T any](db *sql.DB, codec Codec[T], opts SQLiteOptions) (*SQLiteStore[T], error) {
	if db == nil {
		return nil, errors.New("session.NewSQLiteStore: db is required")
	}
	if codec == nil {
		return nil, errors.New("session.NewSQLiteStore: codec is required")
	}
	s := &SQLiteStore[T]{
		db:           db,
		codec:        codec,
		now:          opts.Now,
		strictDecode: opts.StrictDecode,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if opts.AutoCreate {
		if _, err := db.Exec(sqliteSchema); err != nil {
			return nil, fmt.Errorf("session.NewSQLiteStore: apply schema: %w", err)
		}
	}
	return s, nil
}

func (s *SQLiteStore[T]) Load(ctx context.Context, sid string) (Record[T], error) {
	now := s.now().UnixNano()
	row := s.db.QueryRowContext(ctx, `
		SELECT version, user_id, absolute_expiry, idle_expiry, payload
		FROM sessions
		WHERE sid = ?
		  AND (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		sid, now, now)

	var (
		version       uint64
		userID        string
		absExp, idExp int64
		payload       []byte
	)
	if err := row.Scan(&version, &userID, &absExp, &idExp, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record[T]{}, ErrNotFound
		}
		return Record[T]{}, fmt.Errorf("sqlitestore: load %s: %w", sid, err)
	}
	p, err := s.codec.Decode(payload)
	if err != nil {
		if !s.strictDecode {
			// Treat the row as if it never existed. The manager
			// will mint a fresh session on the next write, and
			// the user is silently re-issued a cookie. See
			// [SQLiteOptions.StrictDecode] for the rationale and
			// the opt-out.
			return Record[T]{}, ErrNotFound
		}
		return Record[T]{}, fmt.Errorf("sqlitestore: decode %s: %w", sid, err)
	}
	return Record[T]{
		SID:            sid,
		Version:        version,
		UserID:         userID,
		AbsoluteExpiry: fromUnixNano(absExp),
		IdleExpiry:     fromUnixNano(idExp),
		Payload:        p,
	}, nil
}

func (s *SQLiteStore[T]) Save(ctx context.Context, rec Record[T]) (Record[T], error) {
	if rec.SID == "" {
		return Record[T]{}, errors.New("sqlitestore: save: empty SID")
	}
	payload, err := s.codec.Encode(rec.Payload)
	if err != nil {
		return Record[T]{}, fmt.Errorf("sqlitestore: encode %s: %w", rec.SID, err)
	}
	absExp := toUnixNano(rec.AbsoluteExpiry)
	idExp := toUnixNano(rec.IdleExpiry)

	if rec.Version == 0 {
		// Fresh record. Reject if any other writer has beaten us
		// to this SID — a CAS conflict, not a clobber.
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (sid, version, user_id, absolute_expiry, idle_expiry, payload)
			VALUES (?, 1, ?, ?, ?, ?)
			ON CONFLICT(sid) DO NOTHING`,
			rec.SID, rec.UserID, absExp, idExp, payload)
		if err != nil {
			return Record[T]{}, fmt.Errorf("sqlitestore: insert %s: %w", rec.SID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Record[T]{}, err
		}
		if n == 0 {
			return Record[T]{}, fmt.Errorf("sqlitestore: insert %s collided with existing row: %w", rec.SID, ErrVersionConflict)
		}
		stored := rec
		stored.Version = 1
		return stored, nil
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET version = version + 1,
		    user_id = ?,
		    absolute_expiry = ?,
		    idle_expiry = ?,
		    payload = ?
		WHERE sid = ? AND version = ?`,
		rec.UserID, absExp, idExp, payload, rec.SID, rec.Version)
	if err != nil {
		return Record[T]{}, fmt.Errorf("sqlitestore: update %s: %w", rec.SID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Record[T]{}, err
	}
	if n == 0 {
		return Record[T]{}, fmt.Errorf("sqlitestore: stale write on %s at version %d: %w", rec.SID, rec.Version, ErrVersionConflict)
	}
	stored := rec
	stored.Version++
	return stored, nil
}

func (s *SQLiteStore[T]) Delete(ctx context.Context, sid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE sid = ?`, sid); err != nil {
		return fmt.Errorf("sqlitestore: delete %s: %w", sid, err)
	}
	return nil
}

// BumpTTL implements [TTLBumper]. It updates the idle_expiry column
// without touching version or payload, so concurrent payload writes
// do not see a spurious CAS conflict from a sliding-expiry refresh.
// Returns [ErrNotFound] if no live row exists for sid — the
// expiry filter on the WHERE clause means a row that is past its
// absolute or idle deadline cannot be revived by a stray bump.
func (s *SQLiteStore[T]) BumpTTL(ctx context.Context, sid string, until time.Time) error {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET idle_expiry = ?
		WHERE sid = ?
		  AND (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		toUnixNano(until), sid, now, now)
	if err != nil {
		return fmt.Errorf("sqlitestore: bump %s: %w", sid, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByUser implements [UserIndexer]. Expired rows are filtered out.
func (s *SQLiteStore[T]) ListByUser(ctx context.Context, userID string) ([]string, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT sid FROM sessions
		WHERE user_id = ?
		  AND (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: list user %s: %w", userID, err)
	}
	defer rows.Close()
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	return sids, rows.Err()
}

// RevokeByUser implements [UserIndexer]. SIDs listed in except are
// preserved; everything else for userID is deleted. Returns the
// number of rows removed.
func (s *SQLiteStore[T]) RevokeByUser(ctx context.Context, userID string, except ...string) (int, error) {
	query := `DELETE FROM sessions WHERE user_id = ?`
	args := []any{userID}
	if len(except) > 0 {
		placeholders := strings.Repeat("?,", len(except))
		placeholders = placeholders[:len(placeholders)-1]
		query += ` AND sid NOT IN (` + placeholders + `)`
		for _, sid := range except {
			args = append(args, sid)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: revoke user %s: %w", userID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Scan implements [Scanner]. fn is invoked once per non-expired row;
// iteration stops as soon as fn returns false. Streamed from a
// cursor — large session tables are safe.
func (s *SQLiteStore[T]) Scan(ctx context.Context, fn func(sid string) bool) error {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT sid FROM sessions
		WHERE (absolute_expiry = 0 OR absolute_expiry > ?)
		  AND (idle_expiry     = 0 OR idle_expiry     > ?)`,
		now, now)
	if err != nil {
		return fmt.Errorf("sqlitestore: scan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return err
		}
		if !fn(sid) {
			return nil
		}
	}
	return rows.Err()
}

// Sweep implements [Sweeper]. It deletes every row whose absolute
// or idle expiry has already passed and returns the number removed.
//
// Call this from a scheduler (cron, periodic goroutine in your app,
// recurring job) on roughly the cadence of Config.IdleExpiry —
// sweeping more often is harmless but redundant, and sweeping much
// less often lets expired rows accumulate on disk. Reads already
// filter expired rows, so missing a sweep never returns stale data;
// the only cost of a missed sweep is on-disk size.
func (s *SQLiteStore[T]) Sweep(ctx context.Context) (int, error) {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE (absolute_expiry > 0 AND absolute_expiry <= ?)
		   OR (idle_expiry     > 0 AND idle_expiry     <= ?)`,
		now, now)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func toUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
