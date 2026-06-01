package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// StepState tracks the lifecycle of a persisted step. Only
// [StepSucceeded] rows are written today; failed steps re-run on
// retry. The enum is here so the schema (and future enhancements
// that record per-step retry counts) have a stable vocabulary.
type StepState string

const (
	StepSucceeded StepState = "succeeded"
)

// StepRecord is the persisted result of one successful invocation
// of [Step]. Tied to a job via JobID; (JobID, Name) is unique.
type StepRecord struct {
	ID         string
	JobID      string
	Name       string
	State      StepState
	Result     []byte
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Step runs fn unless this job already completed a step with the
// same name on a prior attempt, in which case the persisted result
// is decoded and returned. The function form (not a method) is
// necessary because Go forbids generic methods on non-generic types.
//
// fn receives the per-attempt context; cancellation propagates as
// usual. The returned T must be JSON-serializable.
//
// When called from outside a job's Run (no jobs state on ctx),
// Step degrades to a plain call of fn so library code that wraps
// arbitrary work in steps still functions under test.
//
// # Void steps
//
// Steps that perform a side effect with no result to pass along
// (sending an email, deleting a file) use T = any and return
// (nil, err):
//
//	_, err := jobs.Step(ctx, "send", func(ctx context.Context) (any, error) {
//	    return nil, sendEmail()
//	})
//
// The discard plus the nil sentinel are two characters of language
// tax for not having implicit unit types; the persistence semantics
// are exactly the same as for value-returning steps.
func Step[T any](ctx Context, name string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if name == "" {
		return zero, fmt.Errorf("jobs.Step: empty name")
	}
	state := fromStdCtx(ctx)
	if state == nil || state.store == nil {
		return fn(ctx)
	}

	rec, err := state.store.GetStep(ctx, state.jobID, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return zero, fmt.Errorf("step %q: load: %w", name, err)
	}
	if rec != nil && rec.State == StepSucceeded {
		if len(rec.Result) == 0 {
			return zero, nil
		}
		if err := json.Unmarshal(rec.Result, &zero); err != nil {
			return zero, fmt.Errorf("step %q: decode persisted result: %w", name, err)
		}
		return zero, nil
	}

	started := time.Now().UTC()
	result, runErr := fn(ctx)
	finished := time.Now().UTC()
	if runErr != nil {
		return result, runErr
	}

	payload, err := json.Marshal(result)
	if err != nil {
		return result, fmt.Errorf("step %q: encode result: %w", name, err)
	}
	saveErr := state.store.SaveStep(ctx, &StepRecord{
		ID:         NewID(),
		JobID:      state.jobID,
		Name:       name,
		State:      StepSucceeded,
		Result:     payload,
		StartedAt:  started,
		FinishedAt: finished,
	})
	if saveErr != nil {
		// A failure to persist the step result is a hard error:
		// without persistence the step is not durable, and silently
		// degrading to "run fn every time" would break user
		// expectations of effectively-once semantics.
		return result, fmt.Errorf("step %q: persist result: %w", name, saveErr)
	}
	return result, nil
}

// ListJobSteps returns the persisted steps for a job, in
// insertion order. Useful for inspection UIs: shows what was
// committed before a crash/retry.
func (m *Manager) ListJobSteps(ctx context.Context, jobID string) ([]StepRecord, error) {
	rows, err := m.store.ListSteps(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]StepRecord, len(rows))
	for i, r := range rows {
		out[i] = *r
	}
	return out, nil
}
