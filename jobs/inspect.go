package jobs

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// JobInfo is the inspection-time view of a job. It is the value
// returned by [Manager.GetJob] and contained in [JobPage.Jobs].
// Distinct from the [Job] interface (which users implement) and
// from [JobRow] (the persistence shape).
type JobInfo struct {
	ID          string
	Kind        string
	Queue       string
	Priority    int
	State       State
	Attempt     int
	MaxAttempts int
	AvailableAt time.Time
	Timeout     time.Duration
	UniqueKey   string
	// Payload is the raw JSON-encoded job struct. Exposed so
	// operators can see what was enqueued without dropping into
	// the database directly. Callers receive a defensive copy.
	Payload []byte
	// Progress holds the latest values reported via ctx.Progress
	// during the most recent run.
	Progress Progress
	// Error is the message persisted at the end of the most recent
	// attempt (success leaves it empty).
	Error string
	// CancelRequested is true when [Manager.CancelJob] flipped the
	// flag on a running job and the worker has not yet observed
	// it. UI can render this as "cancelling..." state.
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Progress captures the most recent progress report from a job's
// Run. Zero values mean "no progress reported yet."
type Progress struct {
	Done  int64
	Total int64
	Msg   string
}

// JobFilter narrows [Manager.ListJobs]. The zero value matches every
// job; Limit defaults to [DefaultJobsLimit] when zero and is clamped
// to [MaxJobsLimit].
type JobFilter struct {
	Queues []string
	Kinds  []string
	States []State
	Limit  int
	Cursor string
}

// JobPage is one page of [Manager.ListJobs] results.
// NextCursor is empty when this is the last page.
type JobPage struct {
	Jobs       []JobInfo
	NextCursor string
}

const (
	DefaultJobsLimit = 100
	MaxJobsLimit     = 1000
)

// EncodeJobsCursor returns the opaque cursor for [Manager.ListJobs]
// pagination, pointing past the given (createdAt, id) tuple.
// Backends call this; callers treat the returned string as opaque.
func EncodeJobsCursor(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeJobsCursor parses a jobs cursor. Empty input yields a zero
// time and empty id, which the caller treats as "no cursor."
func DecodeJobsCursor(c string) (time.Time, string, error) {
	if c == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid jobs cursor: %w", err)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("malformed jobs cursor")
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("jobs cursor timestamp: %w", err)
	}
	return time.Unix(0, nanos), parts[1], nil
}

// NormalizeJobsLimit applies the default and cap for [Manager.ListJobs].
// Backends should call this with the user's Limit before paginating.
func NormalizeJobsLimit(limit int) int {
	if limit <= 0 {
		return DefaultJobsLimit
	}
	if limit > MaxJobsLimit {
		return MaxJobsLimit
	}
	return limit
}
