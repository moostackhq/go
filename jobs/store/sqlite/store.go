// Package sqlite is the SQLite-backed [jobs.Store] implementation.
//
// Single-node only: SQLite's single-writer serialisation makes
// distributed workers across machines unsafe. Use the postgres
// backend for production deployments with multiple worker hosts.
//
// The package depends on modernc.org/sqlite (pure-Go, no CGO),
// matching the convention of the rest of this module family.
//
// Open the database with `?_txlock=immediate&_pragma=journal_mode(WAL)`
// to make every transaction use BEGIN IMMEDIATE and to enable WAL
// journaling for better read concurrency. The [Open] helper does
// this for you.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moostackhq/go/jobs"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Schema returns the DDL that creates every table and index the
// store relies on. Every statement uses IF NOT EXISTS, so calling
// db.Exec(Schema()) on every process start is safe even when the
// schema is also managed externally.
func Schema() string { return schemaSQL }

// Options controls [New]. The zero value disables automatic
// schema creation; callers are expected to apply [Schema] themselves
// (one-shot script, migration tool, ops review).
type Options struct {
	// AutoCreate runs [Schema] on [New].
	AutoCreate bool
}

// Store is the SQLite-backed [jobs.Store].
type Store struct {
	db *sql.DB
}

// New constructs a store bound to db. When opts.AutoCreate is true,
// the embedded DDL is applied immediately.
func New(db *sql.DB, opts ...Options) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("sqlite.New: nil db")
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.AutoCreate {
		if _, err := db.Exec(schemaSQL); err != nil {
			return nil, fmt.Errorf("sqlite.New: apply schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Open is a convenience that opens a SQLite database with the
// connection-string pragmas the store wants: txlock=immediate,
// journal_mode=WAL, foreign_keys=on, busy_timeout=5000ms. Without
// busy_timeout, the second writer in a contended moment fails
// instead of waiting; with it, writes block up to 5s for the lock,
// which is what callers usually want.
//
// Pass a temp file path for tests (":memory:" makes each connection
// in the pool a separate database).
func Open(dsn string) (*sql.DB, error) {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)&_txlock=immediate"
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return sql.Open("sqlite", dsn+sep+pragmas)
}

// --- helpers ---

// execer is the subset of *sql.DB / *sql.Tx the per-method
// implementations need. Lets InsertTx share code with Insert.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is the subset of *sql.Row / *sql.Rows used by the
// scan helpers in this file. Lets one helper handle both
// single-row (QueryRow) and per-row (Query iteration) call sites.
type rowScanner interface {
	Scan(...any) error
}

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// isUniqueConstraint reports whether err is a SQLite UNIQUE
// violation. modernc/sqlite wraps the underlying SQLite error
// string; matching on the substring is portable across driver
// versions.
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}


const insertJobSQL = `
INSERT INTO jobs (
  id, kind, payload, queue, priority, state, attempt, max_attempts,
  available_at, timeout_ms, on_timeout, backoff_spec, unique_key,
  progress_done, progress_total, progress_msg, error,
  locked_by, locked_until, heartbeat_at, cancel_requested,
  created_at, updated_at
) VALUES (
  ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?,
  ?, ?, ?, ?,
  ?, ?, ?, ?,
  ?, ?
)`

func (s *Store) insert(ctx context.Context, e execer, row *jobs.JobRow) error {
	_, err := e.ExecContext(ctx, insertJobSQL,
		row.ID, row.Kind, row.Payload, row.Queue, row.Priority, string(row.State),
		row.Attempt, row.MaxAttempts,
		toUnix(row.AvailableAt), row.TimeoutMs, row.OnTimeoutInt, row.BackoffSpec, row.UniqueKey,
		row.ProgressDone, row.ProgressTotal, row.ProgressMsg, row.Error,
		row.LockedBy, toUnix(row.LockedUntil), toUnix(row.HeartbeatAt),
		boolInt(row.CancelRequested),
		toUnix(row.CreatedAt), toUnix(row.UpdatedAt),
	)
	if err == nil {
		return nil
	}
	if isUniqueConstraint(err) {
		// Look up the existing non-terminal occupant of the key.
		var existing string
		_ = s.db.QueryRowContext(ctx,
			`SELECT id FROM jobs WHERE kind = ? AND unique_key = ?
             AND state NOT IN ('succeeded','failed','cancelled','discarded')
             LIMIT 1`,
			row.Kind, row.UniqueKey).Scan(&existing)
		return &jobs.DuplicateError{
			ExistingID: existing,
			Kind:       row.Kind,
			UniqueKey:  row.UniqueKey,
		}
	}
	return err
}

func (s *Store) Insert(ctx context.Context, row *jobs.JobRow) error {
	return s.insert(ctx, s.db, row)
}

func (s *Store) InsertTx(ctx context.Context, tx any, row *jobs.JobRow) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return jobs.ErrUnsupported
	}
	return s.insert(ctx, sqlTx, row)
}

const selectJobSQL = `
SELECT id, kind, payload, queue, priority, state, attempt, max_attempts,
       available_at, timeout_ms, on_timeout, backoff_spec, unique_key,
       progress_done, progress_total, progress_msg, error,
       locked_by, locked_until, heartbeat_at, cancel_requested,
       created_at, updated_at
FROM jobs`

func scanJob(scanner rowScanner) (*jobs.JobRow, error) {
	var (
		row                jobs.JobRow
		state              string
		availableAt        int64
		lockedUntil        int64
		heartbeatAt        int64
		cancelRequestedInt int64
		createdAt          int64
		updatedAt          int64
	)
	if err := scanner.Scan(
		&row.ID, &row.Kind, &row.Payload, &row.Queue, &row.Priority, &state,
		&row.Attempt, &row.MaxAttempts,
		&availableAt, &row.TimeoutMs, &row.OnTimeoutInt, &row.BackoffSpec, &row.UniqueKey,
		&row.ProgressDone, &row.ProgressTotal, &row.ProgressMsg, &row.Error,
		&row.LockedBy, &lockedUntil, &heartbeatAt, &cancelRequestedInt,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	row.State = jobs.State(state)
	row.AvailableAt = fromUnix(availableAt)
	row.LockedUntil = fromUnix(lockedUntil)
	row.HeartbeatAt = fromUnix(heartbeatAt)
	row.CancelRequested = cancelRequestedInt != 0
	row.CreatedAt = fromUnix(createdAt)
	row.UpdatedAt = fromUnix(updatedAt)
	return &row, nil
}

func (s *Store) Get(ctx context.Context, id string) (*jobs.JobRow, error) {
	row, err := scanJob(s.db.QueryRowContext(ctx, selectJobSQL+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, jobs.ErrNotFound
	}
	return row, err
}

func (s *Store) List(ctx context.Context, f jobs.JobFilter) ([]*jobs.JobRow, string, error) {
	cursorTime, cursorID, err := jobs.DecodeJobsCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := jobs.NormalizeJobsLimit(f.Limit)

	var (
		clauses []string
		args    []any
	)
	if f.Cursor != "" {
		clauses = append(clauses, "(created_at > ? OR (created_at = ? AND id > ?))")
		args = append(args, cursorTime.UnixNano(), cursorTime.UnixNano(), cursorID)
	}
	if len(f.Queues) > 0 {
		clauses = append(clauses, inClause("queue", len(f.Queues)))
		for _, q := range f.Queues {
			args = append(args, q)
		}
	}
	if len(f.Kinds) > 0 {
		clauses = append(clauses, inClause("kind", len(f.Kinds)))
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if len(f.States) > 0 {
		clauses = append(clauses, inClause("state", len(f.States)))
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	q := selectJobSQL
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY created_at ASC, id ASC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	out := make([]*jobs.JobRow, 0, limit+1)
	for rows.Next() {
		r, err := scanJob(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = jobs.EncodeJobsCursor(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return out, next, nil
}

func inClause(col string, n int) string {
	ph := strings.Repeat("?,", n)
	ph = ph[:len(ph)-1]
	return col + " IN (" + ph + ")"
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) Claim(ctx context.Context, req jobs.ClaimRequest) ([]*jobs.JobRow, error) {
	if req.WorkerID == "" {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}

	// BEGIN IMMEDIATE (via the connection string's _txlock=immediate)
	// serialises writers; the select-then-update sequence is safe.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Pre-count running per kind for the global limit check.
	kindRunning := map[string]int{}
	if len(req.KindLimits) > 0 {
		rows, err := tx.QueryContext(ctx,
			"SELECT kind, COUNT(*) FROM jobs WHERE state='running' GROUP BY kind")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var c int
			if err := rows.Scan(&k, &c); err != nil {
				rows.Close()
				return nil, err
			}
			kindRunning[k] = c
		}
		rows.Close()
	}

	// Select candidates: filter by queue and availability.
	var (
		clauses = []string{"state IN ('available','scheduled')", "available_at <= ?"}
		args    = []any{toUnix(req.Now)}
	)
	if len(req.Queues) > 0 {
		clauses = append(clauses, inClause("queue", len(req.Queues)))
		for _, q := range req.Queues {
			args = append(args, q)
		}
	}
	q := selectJobSQL + " WHERE " + strings.Join(clauses, " AND ") +
		" ORDER BY priority DESC, available_at ASC LIMIT ?"
	args = append(args, limit*4) // overfetch to give limits a chance to filter

	rs, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	candidates := make([]*jobs.JobRow, 0)
	for rs.Next() {
		r, err := scanJob(rs)
		if err != nil {
			rs.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	rs.Close()

	until := req.Now.Add(req.LeaseDuration)
	perQueueTaken := map[string]int{}
	taken := make([]*jobs.JobRow, 0, limit)
	for _, c := range candidates {
		if len(taken) >= limit {
			break
		}
		if qLimit, ok := req.QueueLimits[c.Queue]; ok && qLimit >= 0 {
			if perQueueTaken[c.Queue] >= qLimit {
				continue
			}
		}
		if kLimit, ok := req.KindLimits[c.Kind]; ok {
			if kindRunning[c.Kind] >= kLimit {
				continue
			}
		}
		// Lock it.
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state='running', locked_by=?, locked_until=?, heartbeat_at=?, updated_at=?
             WHERE id=? AND state IN ('available','scheduled')`,
			req.WorkerID, toUnix(until), toUnix(req.Now), toUnix(req.Now), c.ID)
		if err != nil {
			return nil, err
		}
		c.State = jobs.StateRunning
		c.LockedBy = req.WorkerID
		c.LockedUntil = until
		c.HeartbeatAt = req.Now
		c.UpdatedAt = req.Now
		taken = append(taken, c)
		perQueueTaken[c.Queue]++
		kindRunning[c.Kind]++
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return taken, nil
}

func (s *Store) Heartbeat(ctx context.Context, jobID, workerID string, until time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var (
		state   string
		owner   string
		cancelI int64
	)
	err = tx.QueryRowContext(ctx,
		"SELECT state, locked_by, cancel_requested FROM jobs WHERE id = ?",
		jobID).Scan(&state, &owner, &cancelI)
	if errors.Is(err, sql.ErrNoRows) {
		return false, jobs.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if state != string(jobs.StateRunning) || owner != workerID {
		return false, jobs.ErrNotFound
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET locked_until = ?, heartbeat_at = ?, updated_at = ?
         WHERE id = ? AND locked_by = ? AND state = 'running'`,
		toUnix(until), toUnix(now), toUnix(now), jobID, workerID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return cancelI != 0, nil
}

func (s *Store) Complete(ctx context.Context, jobID, workerID string, o jobs.Outcome) (jobs.State, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var (
		state           string
		owner           string
		cancelRequested int64
	)
	err = tx.QueryRowContext(ctx,
		"SELECT state, locked_by, cancel_requested FROM jobs WHERE id = ?",
		jobID).Scan(&state, &owner, &cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return "", jobs.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	// Strict ownership: any deviation from "we still hold the lease"
	// is ErrNotFound, including state != running (sweep reclaimed
	// the row while this worker's process was paused).
	if state != string(jobs.StateRunning) || owner != workerID {
		return "", jobs.ErrNotFound
	}

	applied := o.State
	if o.State == jobs.StateSucceeded && cancelRequested != 0 {
		applied = jobs.StateCancelled
	}

	now := time.Now().UTC()
	progressDone, progressTotal, progressMsg := int64(0), int64(0), ""
	usePos := o.FinalProgress != nil
	if usePos {
		progressDone = o.FinalProgress.Done
		progressTotal = o.FinalProgress.Total
		progressMsg = o.FinalProgress.Msg
	}

	if usePos {
		_, err = tx.ExecContext(ctx, `
            UPDATE jobs SET
              state = ?, attempt = ?, error = ?, available_at = ?,
              progress_done = ?, progress_total = ?, progress_msg = ?,
              locked_by = '', locked_until = 0, heartbeat_at = 0, cancel_requested = 0,
              updated_at = ?
            WHERE id = ?`,
			string(applied), o.Attempt, o.Error, toUnix(o.AvailableAt),
			progressDone, progressTotal, progressMsg,
			toUnix(now), jobID)
	} else {
		_, err = tx.ExecContext(ctx, `
            UPDATE jobs SET
              state = ?, attempt = ?, error = ?, available_at = ?,
              locked_by = '', locked_until = 0, heartbeat_at = 0, cancel_requested = 0,
              updated_at = ?
            WHERE id = ?`,
			string(applied), o.Attempt, o.Error, toUnix(o.AvailableAt),
			toUnix(now), jobID)
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return applied, nil
}

// SweepExpired reclaims jobs whose leases expired and writes a
// synthetic Attempt row per reclaimed job so the ledger reflects
// the crashed run instead of leaving a gap.
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, attempt, locked_by, heartbeat_at, locked_until, cancel_requested
           FROM jobs
          WHERE state = 'running'
            AND locked_until != 0
            AND locked_until <= ?`,
		toUnix(now))
	if err != nil {
		return 0, err
	}
	type expired struct {
		id              string
		attempt         int
		lockedBy        string
		heartbeatAt     int64
		lockedUntil     int64
		cancelRequested int64
	}
	var batch []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.attempt, &e.lockedBy, &e.heartbeatAt, &e.lockedUntil, &e.cancelRequested); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, e := range batch {
		attemptNum := e.attempt + 1
		startedAt := e.heartbeatAt
		if startedAt == 0 {
			startedAt = e.lockedUntil
		}
		// Label the synthetic attempt as cancelled when the row
		// carried an unobserved CancelRequested at sweep time, so
		// the ledger reflects the user's intent.
		attemptState := string(jobs.AttemptFailed)
		attemptError := "lease expired"
		if e.cancelRequested != 0 {
			attemptState = string(jobs.AttemptCancelled)
			attemptError = "lease expired during cancellation"
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO job_attempts (id, job_id, attempt, worker_id, started_at, finished_at, state, error)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			jobs.NewID(), e.id, attemptNum, e.lockedBy,
			startedAt, toUnix(now), attemptState, attemptError); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = 'available', attempt = ?, locked_by = '',
                              locked_until = 0, heartbeat_at = 0, updated_at = ?
              WHERE id = ?`,
			attemptNum, toUnix(now), e.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(batch), nil
}


func (s *Store) RecordAttempt(ctx context.Context, a *jobs.Attempt) error {
	if a == nil || a.JobID == "" {
		return jobs.ErrNotFound
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO job_attempts (id, job_id, attempt, worker_id, started_at, finished_at, state, error)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.JobID, a.Attempt, a.WorkerID,
		toUnix(a.StartedAt), toUnix(a.FinishedAt), string(a.State), a.Error)
	return err
}

func (s *Store) ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]*jobs.Attempt, error) {
	if limit <= 0 {
		limit = jobs.DefaultAttemptsLimit + 1
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, job_id, attempt, worker_id, started_at, finished_at, state, error
        FROM job_attempts WHERE job_id = ? AND attempt > ?
        ORDER BY attempt ASC LIMIT ?`, jobID, afterAttempt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.Attempt, 0)
	for rows.Next() {
		var a jobs.Attempt
		var state string
		var startedAt, finishedAt int64
		if err := rows.Scan(&a.ID, &a.JobID, &a.Attempt, &a.WorkerID,
			&startedAt, &finishedAt, &state, &a.Error); err != nil {
			return nil, err
		}
		a.State = jobs.AttemptState(state)
		a.StartedAt = fromUnix(startedAt)
		a.FinishedAt = fromUnix(finishedAt)
		out = append(out, &a)
	}
	return out, rows.Err()
}


func (s *Store) GetStep(ctx context.Context, jobID, name string) (*jobs.StepRecord, error) {
	var (
		r            jobs.StepRecord
		stateStr     string
		startedAt    int64
		finishedAt   int64
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT id, job_id, name, state, result, error, started_at, finished_at
        FROM job_steps WHERE job_id = ? AND name = ?`,
		jobID, name).Scan(&r.ID, &r.JobID, &r.Name, &stateStr, &r.Result, &r.Error,
		&startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, jobs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.State = jobs.StepState(stateStr)
	r.StartedAt = fromUnix(startedAt)
	r.FinishedAt = fromUnix(finishedAt)
	return &r, nil
}

func (s *Store) SaveStep(ctx context.Context, r *jobs.StepRecord) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO job_steps (id, job_id, name, state, result, error, started_at, finished_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.JobID, r.Name, string(r.State), r.Result, r.Error,
		toUnix(r.StartedAt), toUnix(r.FinishedAt))
	if isUniqueConstraint(err) {
		// The runner pre-checks GetStep before SaveStep, so this
		// fires only on a real race between two workers persisting
		// the same step.
		return fmt.Errorf("step %q on job %s already persisted", r.Name, r.JobID)
	}
	return err
}

func (s *Store) ListSteps(ctx context.Context, jobID string) ([]*jobs.StepRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, job_id, name, state, result, error, started_at, finished_at
        FROM job_steps WHERE job_id = ? ORDER BY started_at ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.StepRecord, 0)
	for rows.Next() {
		var (
			r          jobs.StepRecord
			stateStr   string
			startedAt  int64
			finishedAt int64
		)
		if err := rows.Scan(&r.ID, &r.JobID, &r.Name, &stateStr, &r.Result, &r.Error,
			&startedAt, &finishedAt); err != nil {
			return nil, err
		}
		r.State = jobs.StepState(stateStr)
		r.StartedAt = fromUnix(startedAt)
		r.FinishedAt = fromUnix(finishedAt)
		out = append(out, &r)
	}
	return out, rows.Err()
}


func (s *Store) UpdateProgress(ctx context.Context, jobID string, done, total int64, msg string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET progress_done = ?, progress_total = ?, progress_msg = ?, updated_at = ?
         WHERE id = ?`,
		done, total, msg, toUnix(time.Now().UTC()), jobID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return jobs.ErrNotFound
	}
	return nil
}


func (s *Store) Retry(ctx context.Context, jobID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		state                string
		attempt, maxAttempts int
	)
	err = tx.QueryRowContext(ctx,
		"SELECT state, attempt, max_attempts FROM jobs WHERE id = ?", jobID,
	).Scan(&state, &attempt, &maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !jobs.State(state).Terminal() {
		return jobs.ErrJobNotRetryable
	}
	newMax := maxAttempts
	if attempt >= maxAttempts {
		newMax = attempt + 1
	}
	if _, err := tx.ExecContext(ctx, `
        UPDATE jobs SET state = 'available', available_at = ?, max_attempts = ?,
                         error = '', locked_by = '', locked_until = 0, heartbeat_at = 0,
                         cancel_requested = 0, updated_at = ?
         WHERE id = ?`,
		toUnix(now), newMax, toUnix(now), jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Cancel(ctx context.Context, jobID string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx,
		"SELECT state FROM jobs WHERE id = ?", jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, jobs.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	st := jobs.State(state)
	if st.Terminal() {
		return false, jobs.ErrJobTerminal
	}
	if st == jobs.StateRunning {
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET cancel_requested = 1, updated_at = ? WHERE id = ?",
			toUnix(now), jobID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE jobs SET state = 'cancelled', updated_at = ? WHERE id = ?",
		toUnix(now), jobID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) Delete(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx,
		"SELECT state FROM jobs WHERE id = ?", jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if jobs.State(state) == jobs.StateRunning {
		return jobs.ErrJobRunning
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertWorker(ctx context.Context, w *jobs.WorkerRow) error {
	if w == nil || w.ID == "" {
		return jobs.ErrNotFound
	}
	qjson, err := json.Marshal(w.Queues)
	if err != nil {
		return err
	}
	// Preserve original StartedAt on update.
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO workers (id, hostname, queues, started_at, last_seen_at)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            hostname = excluded.hostname,
            queues = excluded.queues,
            last_seen_at = excluded.last_seen_at`,
		w.ID, w.Hostname, string(qjson), toUnix(w.StartedAt), toUnix(w.LastSeenAt))
	return err
}

func (s *Store) RetireWorker(ctx context.Context, workerID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM workers WHERE id = ?", workerID)
	return err
}

func (s *Store) SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM workers WHERE last_seen_at != 0 AND last_seen_at < ?",
		toUnix(olderThan))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) ListWorkers(ctx context.Context) ([]*jobs.WorkerRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, hostname, queues, started_at, last_seen_at FROM workers ORDER BY started_at ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.WorkerRow, 0)
	for rows.Next() {
		var (
			w           jobs.WorkerRow
			qjson       string
			startedAt   int64
			lastSeenAt  int64
		)
		if err := rows.Scan(&w.ID, &w.Hostname, &qjson, &startedAt, &lastSeenAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(qjson), &w.Queues)
		w.StartedAt = fromUnix(startedAt)
		w.LastSeenAt = fromUnix(lastSeenAt)
		out = append(out, &w)
	}
	return out, rows.Err()
}

func (s *Store) ListQueues(ctx context.Context) ([]jobs.QueueInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT queue, state, COUNT(*) FROM jobs GROUP BY queue, state ORDER BY queue ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*jobs.QueueInfo{}
	var order []string
	for rows.Next() {
		var (
			queue, state string
			count        int
		)
		if err := rows.Scan(&queue, &state, &count); err != nil {
			return nil, err
		}
		info, ok := byName[queue]
		if !ok {
			info = &jobs.QueueInfo{Name: queue, Counts: map[jobs.State]int{}}
			byName[queue] = info
			order = append(order, queue)
		}
		info.Counts[jobs.State(state)] = count
	}
	out := make([]jobs.QueueInfo, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, rows.Err()
}


func (s *Store) UpsertSchedule(ctx context.Context, sched *jobs.ScheduleRow) error {
	if sched == nil || sched.Name == "" {
		return jobs.ErrNotFound
	}
	// On conflict, preserve next_run_at when the cron expression
	// did not change: re-calling Schedule on every boot would
	// otherwise silently swallow missed ticks across downtime and
	// break the CatchUp contract.
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO schedules (name, kind, cron, payload, options, next_run_at, last_run_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(name) DO UPDATE SET
            kind = excluded.kind,
            cron = excluded.cron,
            payload = excluded.payload,
            options = excluded.options,
            next_run_at = CASE
                WHEN schedules.cron = excluded.cron THEN schedules.next_run_at
                ELSE excluded.next_run_at
            END,
            updated_at = excluded.updated_at`,
		sched.Name, sched.Kind, sched.Cron, sched.Payload, sched.OptionsJSON,
		toUnix(sched.NextRunAt), toUnix(sched.LastRunAt), toUnix(sched.UpdatedAt))
	return err
}

func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM schedules WHERE name = ?", name)
	return err
}

const selectScheduleSQL = `
SELECT name, kind, cron, payload, options, next_run_at, last_run_at, updated_at
FROM schedules`

func scanSchedule(scanner rowScanner) (*jobs.ScheduleRow, error) {
	var (
		sched      jobs.ScheduleRow
		nextRunAt  int64
		lastRunAt  int64
		updatedAt  int64
	)
	if err := scanner.Scan(&sched.Name, &sched.Kind, &sched.Cron, &sched.Payload, &sched.OptionsJSON,
		&nextRunAt, &lastRunAt, &updatedAt); err != nil {
		return nil, err
	}
	sched.NextRunAt = fromUnix(nextRunAt)
	sched.LastRunAt = fromUnix(lastRunAt)
	sched.UpdatedAt = fromUnix(updatedAt)
	return &sched, nil
}

func (s *Store) ListSchedules(ctx context.Context) ([]*jobs.ScheduleRow, error) {
	rows, err := s.db.QueryContext(ctx, selectScheduleSQL+" ORDER BY name ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.ScheduleRow, 0)
	for rows.Next() {
		r, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]*jobs.ScheduleRow, error) {
	rows, err := s.db.QueryContext(ctx,
		selectScheduleSQL+" WHERE next_run_at != 0 AND next_run_at <= ? ORDER BY next_run_at ASC",
		toUnix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.ScheduleRow, 0)
	for rows.Next() {
		r, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ClaimSchedule(ctx context.Context, name string, expectedLast, newLast, newNext time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
        UPDATE schedules SET last_run_at = ?, next_run_at = ?, updated_at = ?
        WHERE name = ? AND last_run_at = ?`,
		toUnix(newLast), toUnix(newNext), toUnix(newLast),
		name, toUnix(expectedLast))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
