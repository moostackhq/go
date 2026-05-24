// Package migrations applies up-only SQL migrations from an fs.FS to a *sql.DB.
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors. All errors returned by Migrate that fall into one of
// these categories wrap the corresponding sentinel, so callers can use
// errors.Is to dispatch on failure mode.
var (
	// ErrDrift is wrapped when an applied migration's stored checksum no
	// longer matches the file body in source.
	ErrDrift = errors.New("migration drift")

	// ErrOrphan is wrapped when schema_migrations records a version that
	// no longer exists in source.
	ErrOrphan = errors.New("orphan migration")

	// ErrInvalidFilename is wrapped when a .sql file in source does not
	// match the NNNN_name.sql pattern.
	ErrInvalidFilename = errors.New("invalid migration filename")

	// ErrDuplicateVersion is wrapped when two source files share the same
	// numeric version prefix.
	ErrDuplicateVersion = errors.New("duplicate migration version")
)

type Dialect int

const (
	DialectSQLite Dialect = iota
	DialectPostgres
)

func (d Dialect) String() string {
	switch d {
	case DialectSQLite:
		return "sqlite"
	case DialectPostgres:
		return "postgres"
	}
	return fmt.Sprintf("Dialect(%d)", int(d))
}

type State int

const (
	StatePending State = iota
	StateApplied
	StateDrifted
	StateOrphan
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateApplied:
		return "applied"
	case StateDrifted:
		return "drifted"
	case StateOrphan:
		return "orphan"
	}
	return "unknown"
}

type MigrationStatus struct {
	Version int64
	Name    string
	// Checksum is the value stored in schema_migrations. Empty when the
	// migration has no row yet (StatePending).
	Checksum string
	// FileChecksum is the SHA-256 of the migration file in source. Empty
	// when the source file is missing (StateOrphan). For StateDrifted this
	// is the on-disk value that no longer matches Checksum.
	FileChecksum string
	AppliedAt    time.Time
	State        State
}

type migration struct {
	version  int64
	name     string
	body     string
	checksum string
}

type appliedRow struct {
	name      string
	checksum  string
	appliedAt time.Time
}

var filenameRE = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_-]+)\.sql$`)

// querier is satisfied by both *sql.DB and *sql.Conn — used by helpers that
// must work whether the caller has acquired a dedicated connection or not.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Migrate applies every pending migration found in source to db, in version
// order. Re-running is idempotent.
//
// Each migration commits independently. If a migration fails, earlier
// migrations in the same Migrate call remain applied, the failing
// migration is rolled back, and later migrations are skipped. Per-engine
// locking strategy is documented on the corresponding driver.
//
// Migrate verifies that every applied migration's stored checksum still
// matches the file body, and that every applied migration still exists in
// source. Drift or missing files cause Migrate to error before any new
// migration runs.
func Migrate(ctx context.Context, db *sql.DB, dialect Dialect, source fs.FS) (rerr error) {
	drv, err := driverFor(dialect)
	if err != nil {
		return err
	}

	migs, err := loadMigrations(source)
	if err != nil {
		return err
	}

	sess, err := drv.openSession(ctx, db)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := sess.close(); cerr != nil && rerr == nil {
			rerr = cerr
		}
	}()

	if _, err := sess.ExecContext(ctx, drv.schemaSQL()); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, sess)
	if err != nil {
		return err
	}

	migsByVersion := make(map[int64]migration, len(migs))
	for _, m := range migs {
		migsByVersion[m.version] = m
	}
	for version, row := range applied {
		m, inSource := migsByVersion[version]
		if !inSource {
			return fmt.Errorf("migration %d_%s is recorded as applied but missing from source: %w", version, row.name, ErrOrphan)
		}
		if row.checksum != m.checksum {
			return fmt.Errorf("migration %d_%s has changed since it was applied (file checksum %s, stored %s): %w", m.version, m.name, m.checksum, row.checksum, ErrDrift)
		}
	}

	insertSQL := "INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (" + placeholders(drv, 4) + ")"

	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := sess.apply(ctx, m, insertSQL); err != nil {
			return fmt.Errorf("apply migration %d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// Status reports the state of every migration the runner can see, both in
// source and in the database. The returned slice is sorted by Version.
// Status is read-only and acquires no lock; it requires schema_migrations
// to already exist (i.e. a prior Migrate call). A snapshot taken while a
// Migrate call is in progress may be transient.
func Status(ctx context.Context, db *sql.DB, source fs.FS) ([]MigrationStatus, error) {
	migs, err := loadMigrations(source)
	if err != nil {
		return nil, err
	}

	applied, err := loadApplied(ctx, db)
	if err != nil {
		return nil, err
	}

	out := make([]MigrationStatus, 0, len(migs)+len(applied))
	inSource := make(map[int64]bool, len(migs))
	for _, m := range migs {
		inSource[m.version] = true
		row, ok := applied[m.version]
		if !ok {
			out = append(out, MigrationStatus{
				Version:      m.version,
				Name:         m.name,
				FileChecksum: m.checksum,
				State:        StatePending,
			})
			continue
		}
		state := StateApplied
		if row.checksum != m.checksum {
			state = StateDrifted
		}
		out = append(out, MigrationStatus{
			Version:      m.version,
			Name:         m.name,
			Checksum:     row.checksum,
			FileChecksum: m.checksum,
			AppliedAt:    row.appliedAt,
			State:        state,
		})
	}

	for version, row := range applied {
		if inSource[version] {
			continue
		}
		out = append(out, MigrationStatus{
			Version:   version,
			Name:      row.name,
			Checksum:  row.checksum,
			AppliedAt: row.appliedAt,
			State:     StateOrphan,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func loadMigrations(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}

	var migs []migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		match := filenameRE.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%q (want NNNN_name.sql): %w", name, ErrInvalidFilename)
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid version in %q: %w", name, err)
		}
		if prev, ok := seen[version]; ok {
			return nil, fmt.Errorf("version %d in %q and %q: %w", version, prev, name, ErrDuplicateVersion)
		}
		seen[version] = name
		body, err := fs.ReadFile(source, name)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", name, err)
		}
		sum := sha256.Sum256(body)
		migs = append(migs, migration{
			version:  version,
			name:     match[2],
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

func loadApplied(ctx context.Context, q querier) (map[int64]appliedRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT version, name, checksum, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int64]appliedRow{}
	for rows.Next() {
		var v int64
		var r appliedRow
		if err := rows.Scan(&v, &r.name, &r.checksum, &r.appliedAt); err != nil {
			return nil, err
		}
		applied[v] = r
	}
	return applied, rows.Err()
}
