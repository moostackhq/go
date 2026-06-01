package jobs

import (
	"context"
	"time"
)

// RetryJob resets a terminal job (failed, discarded, or cancelled)
// to available so it can be picked up again. Returns
// [ErrJobNotRetryable] when the job is in a non-terminal state.
//
// The attempt counter is preserved (so [Manager.ListJobAttempts]
// retains history), and one additional attempt is permitted by
// bumping MaxAttempts to Attempt+1 when at the cap.
func (m *Manager) RetryJob(ctx context.Context, id string) error {
	return m.store.Retry(ctx, id, time.Now().UTC())
}

// CancelJob requests cancellation. Behaviour depends on current state:
//
//   - scheduled or available: transitioned to [StateCancelled]
//     immediately; the returned immediate is true.
//   - running: a cancellation flag is set on the job row; the worker
//     observes it on the next heartbeat (within HeartbeatInterval),
//     cancels the job's context, and writes the cancelled outcome
//     when Run returns. The returned immediate is false; UIs can
//     render "cancelling..." until a subsequent [Manager.GetJob]
//     confirms the terminal transition.
//   - terminal: returns [ErrJobTerminal].
//
// Callers that don't care about the synchronous/deferred distinction
// can ignore the first return value with `_, err := mgr.CancelJob(...)`.
func (m *Manager) CancelJob(ctx context.Context, id string) (immediate bool, err error) {
	return m.store.Cancel(ctx, id, time.Now().UTC())
}

// DeleteJob removes a job and its attempt and step history.
// Returns [ErrJobRunning] if the job is currently leased; cancel
// first, wait for the worker to release the lease, then delete.
func (m *Manager) DeleteJob(ctx context.Context, id string) error {
	return m.store.Delete(ctx, id)
}

// ListWorkers returns the inspection view of every currently-alive
// worker, ordered by start time.
func (m *Manager) ListWorkers(ctx context.Context) ([]WorkerInfo, error) {
	rows, err := m.store.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WorkerInfo, len(rows))
	for i, r := range rows {
		out[i] = WorkerInfo{
			ID:         r.ID,
			Hostname:   r.Hostname,
			Queues:     append([]string(nil), r.Queues...),
			StartedAt:  r.StartedAt,
			LastSeenAt: r.LastSeenAt,
		}
	}
	return out, nil
}

// ListQueues returns one entry per distinct queue with counts per
// state.
func (m *Manager) ListQueues(ctx context.Context) ([]QueueInfo, error) {
	return m.store.ListQueues(ctx)
}

// WorkerInfo is the inspection-time view of a worker row.
type WorkerInfo struct {
	ID         string
	Hostname   string
	Queues     []string
	StartedAt  time.Time
	LastSeenAt time.Time
}
