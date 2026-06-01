// Package postgres is the PostgreSQL-backed [jobs.Store] implementation.
//
// Production backend: supports concurrent workers across machines
// via SELECT ... FOR UPDATE SKIP LOCKED on the claim path.
//
// The package depends on github.com/lib/pq; users open their own
// *sql.DB and pass it to [New].
package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/moostackhq/go/jobs"
)

//go:embed schema.sql
var schemaSQL string

// Schema returns the DDL.
func Schema() string { return schemaSQL }

// Options controls [New]. The zero value disables automatic
// schema creation.
type Options struct {
	AutoCreate bool
}

// Store is the PostgreSQL-backed [jobs.Store].
type Store struct {
	db *sql.DB
}

// New constructs a store bound to db. When opts.AutoCreate is true,
// [Schema] is applied immediately.
func New(db *sql.DB, opts ...Options) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("postgres.New: nil db")
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.AutoCreate {
		if _, err := db.Exec(schemaSQL); err != nil {
			return nil, fmt.Errorf("postgres.New: apply schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// --- helpers ---

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is the subset of *sql.Row / *sql.Rows used by the
// scan helpers in this file.
type rowScanner interface {
	Scan(...any) error
}

// isUniqueConstraint reports whether err is a Postgres UNIQUE
// violation (SQLSTATE 23505).
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

func fromNullTime(n sql.NullTime) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return n.Time.UTC()
}

const insertJobSQL = `
INSERT INTO jobs (
  id, kind, payload, queue, priority, state, attempt, max_attempts,
  available_at, timeout_ms, on_timeout, backoff_spec, unique_key,
  progress_done, progress_total, progress_msg, error,
  locked_by, locked_until, heartbeat_at, cancel_requested,
  created_at, updated_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8,
  $9, $10, $11, $12, $13,
  $14, $15, $16, $17,
  $18, $19, $20, $21,
  $22, $23
)`

func (s *Store) insert(ctx context.Context, e execer, row *jobs.JobRow) error {
	_, err := e.ExecContext(ctx, insertJobSQL,
		row.ID, row.Kind, row.Payload, row.Queue, row.Priority, string(row.State),
		row.Attempt, row.MaxAttempts,
		row.AvailableAt, row.TimeoutMs, row.OnTimeoutInt, row.BackoffSpec, row.UniqueKey,
		row.ProgressDone, row.ProgressTotal, row.ProgressMsg, row.Error,
		row.LockedBy, nullTime(row.LockedUntil), nullTime(row.HeartbeatAt),
		row.CancelRequested,
		row.CreatedAt, row.UpdatedAt,
	)
	if err == nil {
		return nil
	}
	if isUniqueConstraint(err) {
		var existing string
		_ = s.db.QueryRowContext(ctx,
			`SELECT id FROM jobs WHERE kind = $1 AND unique_key = $2
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
		row         jobs.JobRow
		stateStr    string
		lockedUntil sql.NullTime
		heartbeatAt sql.NullTime
	)
	if err := scanner.Scan(
		&row.ID, &row.Kind, &row.Payload, &row.Queue, &row.Priority, &stateStr,
		&row.Attempt, &row.MaxAttempts,
		&row.AvailableAt, &row.TimeoutMs, &row.OnTimeoutInt, &row.BackoffSpec, &row.UniqueKey,
		&row.ProgressDone, &row.ProgressTotal, &row.ProgressMsg, &row.Error,
		&row.LockedBy, &lockedUntil, &heartbeatAt, &row.CancelRequested,
		&row.CreatedAt, &row.UpdatedAt,
	); err != nil {
		return nil, err
	}
	row.State = jobs.State(stateStr)
	row.AvailableAt = row.AvailableAt.UTC()
	row.CreatedAt = row.CreatedAt.UTC()
	row.UpdatedAt = row.UpdatedAt.UTC()
	row.LockedUntil = fromNullTime(lockedUntil)
	row.HeartbeatAt = fromNullTime(heartbeatAt)
	return &row, nil
}

func (s *Store) Get(ctx context.Context, id string) (*jobs.JobRow, error) {
	row, err := scanJob(s.db.QueryRowContext(ctx, selectJobSQL+" WHERE id = $1", id))
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
	add := func(s string, a ...any) {
		clauses = append(clauses, s)
		args = append(args, a...)
	}
	ph := func() int { return len(args) }

	if f.Cursor != "" {
		add("(created_at > $"+strconv.Itoa(ph()+1)+" OR (created_at = $"+strconv.Itoa(ph()+2)+" AND id > $"+strconv.Itoa(ph()+3)+"))",
			cursorTime, cursorTime, cursorID)
	}
	if len(f.Queues) > 0 {
		add("queue = ANY($"+strconv.Itoa(ph()+1)+")", pq.StringArray(f.Queues))
	}
	if len(f.Kinds) > 0 {
		add("kind = ANY($"+strconv.Itoa(ph()+1)+")", pq.StringArray(f.Kinds))
	}
	if len(f.States) > 0 {
		ss := make([]string, len(f.States))
		for i, st := range f.States {
			ss[i] = string(st)
		}
		add("state = ANY($"+strconv.Itoa(ph()+1)+")", pq.StringArray(ss))
	}

	q := selectJobSQL
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY created_at ASC, id ASC LIMIT $" + strconv.Itoa(ph()+1)
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

func (s *Store) Claim(ctx context.Context, req jobs.ClaimRequest) ([]*jobs.JobRow, error) {
	if req.WorkerID == "" {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

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

	// SELECT FOR UPDATE SKIP LOCKED is the Postgres-specific
	// concurrent claim primitive: rows held by another transaction
	// are not blocking, they are just skipped.
	q := selectJobSQL + ` WHERE state IN ('available','scheduled') AND available_at <= $1`
	args := []any{req.Now}
	if len(req.Queues) > 0 {
		q += " AND queue = ANY($2)"
		args = append(args, pq.StringArray(req.Queues))
	}
	q += " ORDER BY priority DESC, available_at ASC LIMIT $" + strconv.Itoa(len(args)+1) + " FOR UPDATE SKIP LOCKED"
	args = append(args, limit*4)

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
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state='running', locked_by=$1, locked_until=$2,
                 heartbeat_at=$3, updated_at=$4
             WHERE id=$5`,
			req.WorkerID, until, req.Now, req.Now, c.ID)
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
		state           string
		owner           string
		cancelRequested bool
	)
	err = tx.QueryRowContext(ctx,
		"SELECT state, locked_by, cancel_requested FROM jobs WHERE id = $1",
		jobID).Scan(&state, &owner, &cancelRequested)
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
		`UPDATE jobs SET locked_until = $1, heartbeat_at = $2, updated_at = $3
         WHERE id = $4 AND locked_by = $5 AND state = 'running'`,
		until, now, now, jobID, workerID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return cancelRequested, nil
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
		cancelRequested bool
	)
	err = tx.QueryRowContext(ctx,
		"SELECT state, locked_by, cancel_requested FROM jobs WHERE id = $1",
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
	if o.State == jobs.StateSucceeded && cancelRequested {
		applied = jobs.StateCancelled
	}

	now := time.Now().UTC()
	if o.FinalProgress != nil {
		_, err = tx.ExecContext(ctx, `
            UPDATE jobs SET
              state = $1, attempt = $2, error = $3, available_at = $4,
              progress_done = $5, progress_total = $6, progress_msg = $7,
              locked_by = '', locked_until = NULL, heartbeat_at = NULL,
              cancel_requested = FALSE, updated_at = $8
            WHERE id = $9`,
			string(applied), o.Attempt, o.Error, o.AvailableAt,
			o.FinalProgress.Done, o.FinalProgress.Total, o.FinalProgress.Msg,
			now, jobID)
	} else {
		_, err = tx.ExecContext(ctx, `
            UPDATE jobs SET
              state = $1, attempt = $2, error = $3, available_at = $4,
              locked_by = '', locked_until = NULL, heartbeat_at = NULL,
              cancel_requested = FALSE, updated_at = $5
            WHERE id = $6`,
			string(applied), o.Attempt, o.Error, o.AvailableAt,
			now, jobID)
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

	rows, err := tx.QueryContext(ctx, `
        SELECT id, attempt, locked_by, heartbeat_at, locked_until, cancel_requested
          FROM jobs
         WHERE state = 'running'
           AND locked_until IS NOT NULL
           AND locked_until <= $1
         FOR UPDATE SKIP LOCKED`,
		now)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id              string
		attempt         int
		lockedBy        string
		heartbeatAt     sql.NullTime
		lockedUntil     sql.NullTime
		cancelRequested bool
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
		startedAt := fromNullTime(e.heartbeatAt)
		if startedAt.IsZero() {
			startedAt = fromNullTime(e.lockedUntil)
		}
		// Label the synthetic attempt as cancelled when the row
		// carried an unobserved CancelRequested at sweep time, so
		// the ledger reflects the user's intent.
		attemptState := string(jobs.AttemptFailed)
		attemptError := "lease expired"
		if e.cancelRequested {
			attemptState = string(jobs.AttemptCancelled)
			attemptError = "lease expired during cancellation"
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO job_attempts (id, job_id, attempt, worker_id, started_at, finished_at, state, error)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			jobs.NewID(), e.id, attemptNum, e.lockedBy,
			startedAt, nullTime(now), attemptState, attemptError); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
            UPDATE jobs SET state = 'available', attempt = $1, locked_by = '',
                              locked_until = NULL, heartbeat_at = NULL, updated_at = $2
              WHERE id = $3`,
			attemptNum, now, e.id); err != nil {
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
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.ID, a.JobID, a.Attempt, a.WorkerID,
		a.StartedAt, nullTime(a.FinishedAt), string(a.State), a.Error)
	return err
}

func (s *Store) ListAttempts(ctx context.Context, jobID string, afterAttempt, limit int) ([]*jobs.Attempt, error) {
	if limit <= 0 {
		limit = jobs.DefaultAttemptsLimit + 1
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, job_id, attempt, worker_id, started_at, finished_at, state, error
        FROM job_attempts WHERE job_id = $1 AND attempt > $2
        ORDER BY attempt ASC LIMIT $3`, jobID, afterAttempt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.Attempt, 0)
	for rows.Next() {
		var (
			a          jobs.Attempt
			state      string
			finishedAt sql.NullTime
		)
		if err := rows.Scan(&a.ID, &a.JobID, &a.Attempt, &a.WorkerID,
			&a.StartedAt, &finishedAt, &state, &a.Error); err != nil {
			return nil, err
		}
		a.State = jobs.AttemptState(state)
		a.StartedAt = a.StartedAt.UTC()
		a.FinishedAt = fromNullTime(finishedAt)
		out = append(out, &a)
	}
	return out, rows.Err()
}


func (s *Store) GetStep(ctx context.Context, jobID, name string) (*jobs.StepRecord, error) {
	var (
		r          jobs.StepRecord
		stateStr   string
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT id, job_id, name, state, result, error, started_at, finished_at
        FROM job_steps WHERE job_id = $1 AND name = $2`,
		jobID, name).Scan(&r.ID, &r.JobID, &r.Name, &stateStr, &r.Result, &r.Error,
		&startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, jobs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.State = jobs.StepState(stateStr)
	r.StartedAt = fromNullTime(startedAt)
	r.FinishedAt = fromNullTime(finishedAt)
	return &r, nil
}

func (s *Store) SaveStep(ctx context.Context, r *jobs.StepRecord) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO job_steps (id, job_id, name, state, result, error, started_at, finished_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.ID, r.JobID, r.Name, string(r.State), r.Result, r.Error,
		nullTime(r.StartedAt), nullTime(r.FinishedAt))
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
        FROM job_steps WHERE job_id = $1 ORDER BY started_at ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*jobs.StepRecord, 0)
	for rows.Next() {
		var (
			r          jobs.StepRecord
			stateStr   string
			startedAt  sql.NullTime
			finishedAt sql.NullTime
		)
		if err := rows.Scan(&r.ID, &r.JobID, &r.Name, &stateStr, &r.Result, &r.Error,
			&startedAt, &finishedAt); err != nil {
			return nil, err
		}
		r.State = jobs.StepState(stateStr)
		r.StartedAt = fromNullTime(startedAt)
		r.FinishedAt = fromNullTime(finishedAt)
		out = append(out, &r)
	}
	return out, rows.Err()
}


func (s *Store) UpdateProgress(ctx context.Context, jobID string, done, total int64, msg string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET progress_done = $1, progress_total = $2, progress_msg = $3, updated_at = $4
         WHERE id = $5`,
		done, total, msg, time.Now().UTC(), jobID)
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
		"SELECT state, attempt, max_attempts FROM jobs WHERE id = $1", jobID,
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
        UPDATE jobs SET state = 'available', available_at = $1, max_attempts = $2,
                         error = '', locked_by = '', locked_until = NULL, heartbeat_at = NULL,
                         cancel_requested = FALSE, updated_at = $3
         WHERE id = $4`,
		now, newMax, now, jobID); err != nil {
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
		"SELECT state FROM jobs WHERE id = $1", jobID).Scan(&state)
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
			"UPDATE jobs SET cancel_requested = TRUE, updated_at = $1 WHERE id = $2",
			now, jobID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE jobs SET state = 'cancelled', updated_at = $1 WHERE id = $2",
		now, jobID); err != nil {
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
		"SELECT state FROM jobs WHERE id = $1", jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if jobs.State(state) == jobs.StateRunning {
		return jobs.ErrJobRunning
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", jobID); err != nil {
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
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO workers (id, hostname, queues, started_at, last_seen_at)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (id) DO UPDATE SET
            hostname = EXCLUDED.hostname,
            queues = EXCLUDED.queues,
            last_seen_at = EXCLUDED.last_seen_at`,
		w.ID, w.Hostname, string(qjson), w.StartedAt, w.LastSeenAt)
	return err
}

func (s *Store) RetireWorker(ctx context.Context, workerID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM workers WHERE id = $1", workerID)
	return err
}

func (s *Store) SweepStaleWorkers(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM workers WHERE last_seen_at < $1", olderThan)
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
			w     jobs.WorkerRow
			qjson string
		)
		if err := rows.Scan(&w.ID, &w.Hostname, &qjson, &w.StartedAt, &w.LastSeenAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(qjson), &w.Queues)
		w.StartedAt = w.StartedAt.UTC()
		w.LastSeenAt = w.LastSeenAt.UTC()
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
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (name) DO UPDATE SET
            kind = EXCLUDED.kind,
            cron = EXCLUDED.cron,
            payload = EXCLUDED.payload,
            options = EXCLUDED.options,
            next_run_at = CASE
                WHEN schedules.cron = EXCLUDED.cron THEN schedules.next_run_at
                ELSE EXCLUDED.next_run_at
            END,
            updated_at = EXCLUDED.updated_at`,
		sched.Name, sched.Kind, sched.Cron, sched.Payload, sched.OptionsJSON,
		nullTime(sched.NextRunAt), nullTime(sched.LastRunAt), sched.UpdatedAt)
	return err
}

func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM schedules WHERE name = $1", name)
	return err
}

const selectScheduleSQL = `
SELECT name, kind, cron, payload, options, next_run_at, last_run_at, updated_at
FROM schedules`

func scanSchedule(scanner rowScanner) (*jobs.ScheduleRow, error) {
	var (
		sched     jobs.ScheduleRow
		nextRunAt sql.NullTime
		lastRunAt sql.NullTime
	)
	if err := scanner.Scan(&sched.Name, &sched.Kind, &sched.Cron, &sched.Payload, &sched.OptionsJSON,
		&nextRunAt, &lastRunAt, &sched.UpdatedAt); err != nil {
		return nil, err
	}
	sched.NextRunAt = fromNullTime(nextRunAt)
	sched.LastRunAt = fromNullTime(lastRunAt)
	sched.UpdatedAt = sched.UpdatedAt.UTC()
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
		selectScheduleSQL+" WHERE next_run_at IS NOT NULL AND next_run_at <= $1 ORDER BY next_run_at ASC",
		now)
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
	// Match-against-NULL needs IS NOT DISTINCT FROM semantics
	// because NULL = NULL is unknown. Use explicit handling.
	var res sql.Result
	var err error
	if expectedLast.IsZero() {
		res, err = s.db.ExecContext(ctx, `
            UPDATE schedules SET last_run_at = $1, next_run_at = $2, updated_at = $3
            WHERE name = $4 AND last_run_at IS NULL`,
			newLast, newNext, newLast, name)
	} else {
		res, err = s.db.ExecContext(ctx, `
            UPDATE schedules SET last_run_at = $1, next_run_at = $2, updated_at = $3
            WHERE name = $4 AND last_run_at = $5`,
			newLast, newNext, newLast, name, expectedLast)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
