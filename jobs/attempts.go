package jobs

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"
)

// AttemptState is the outcome of a single attempt at running a job.
// Persisted in the `job_attempts` table; rendered by ListJobAttempts.
//
// Note: this differs from the job's [State]. A job's State tracks its
// position in the lifecycle (scheduled, running, succeeded, ...);
// an Attempt's State records how a single Run invocation ended.
// A discarded job may have N failed attempts; a retried job may have
// N timed_out attempts followed by one succeeded.
type AttemptState string

const (
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptTimedOut  AttemptState = "timed_out"
	AttemptCancelled AttemptState = "cancelled"
)

// Attempt is one row of the `job_attempts` ledger: every invocation
// of [Job.Run] produces exactly one of these, written by the runner
// just before [Manager.GetJob] flips to the terminal state.
type Attempt struct {
	ID         string
	JobID      string
	Attempt    int
	WorkerID   string
	StartedAt  time.Time
	FinishedAt time.Time
	State      AttemptState
	Error      string
}

// AttemptsFilter narrows [Manager.ListJobAttempts] pagination. The
// zero value returns the first [DefaultAttemptsLimit] attempts.
type AttemptsFilter struct {
	// Limit caps the page size. Zero defaults to [DefaultAttemptsLimit];
	// values above [MaxAttemptsLimit] are clamped.
	Limit int
	// Cursor is an opaque token returned by the previous call's
	// [AttemptsPage.NextCursor]. Empty means "first page."
	Cursor string
}

// AttemptsPage is one page of [Manager.ListJobAttempts] results.
// NextCursor is empty when this is the last page.
type AttemptsPage struct {
	Attempts   []Attempt
	NextCursor string
}

const (
	DefaultAttemptsLimit = 100
	MaxAttemptsLimit     = 1000
)

// NormalizeAttemptsLimit clamps an [AttemptsFilter.Limit] to the
// configured bounds.
func NormalizeAttemptsLimit(limit int) int {
	if limit <= 0 {
		return DefaultAttemptsLimit
	}
	if limit > MaxAttemptsLimit {
		return MaxAttemptsLimit
	}
	return limit
}

// EncodeAttemptsCursor returns the opaque cursor for
// [Manager.ListJobAttempts] pagination, pointing past the given
// attempt number. Backends call this; callers treat the returned
// string as opaque.
func EncodeAttemptsCursor(attempt int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(attempt)))
}

// DecodeAttemptsCursor parses an attempts cursor. Empty input
// yields 0 (the caller treats this as "no cursor, start from the
// beginning").
func DecodeAttemptsCursor(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, fmt.Errorf("invalid attempts cursor: %w", err)
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, fmt.Errorf("attempts cursor: %w", err)
	}
	return n, nil
}

// ListJobAttempts returns one page of the attempt history for a
// job, oldest first. Pagination is rarely needed in practice
// (MaxAttempts defaults to 25), but RetryJob can push the count up
// over time, so the API is cursor-paginated to bound any single call.
//
// Empty page (no error) when the job has not yet been attempted, or
// when the job does not exist.
func (m *Manager) ListJobAttempts(ctx context.Context, jobID string, f AttemptsFilter) (AttemptsPage, error) {
	afterAttempt, err := DecodeAttemptsCursor(f.Cursor)
	if err != nil {
		return AttemptsPage{}, err
	}
	limit := NormalizeAttemptsLimit(f.Limit)

	// Fetch limit+1 to detect "more rows exist" without a second
	// round trip; trim and encode the cursor from the limit-th row.
	rows, err := m.store.ListAttempts(ctx, jobID, afterAttempt, limit+1)
	if err != nil {
		return AttemptsPage{}, err
	}

	page := AttemptsPage{}
	if len(rows) > limit {
		last := rows[limit-1]
		page.NextCursor = EncodeAttemptsCursor(last.Attempt)
		rows = rows[:limit]
	}
	page.Attempts = make([]Attempt, len(rows))
	for i, r := range rows {
		page.Attempts[i] = *r
	}
	return page, nil
}
