// Package memory is the non-durable in-process [jobs.Store] backend,
// intended for tests, examples, and local development. State does
// not survive process restart.
//
// The implementation is intentionally simple: a single mutex, a map
// keyed by id, and on-demand sorting for List. This is fine at the
// scales the memory backend exists for; production deployments
// should use the sqlite or postgres backends.
package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/moostackhq/go/jobs"
)

// Store is the in-memory [jobs.Store] implementation. The zero
// value is not usable; construct one with [New].
type Store struct {
	mu sync.RWMutex
	// rows holds every job ever inserted, keyed by id. Defensive
	// copies are taken on read and write so callers cannot mutate
	// the canonical row.
	rows map[string]*jobs.JobRow
	// uniqueIndex maps "<kind>\x00<uniquekey>" to the live job
	// holding that combination. Stale entries are evicted on read.
	uniqueIndex map[string]string
	// attempts is the per-job append-only ledger of [jobs.Attempt]
	// rows, oldest first.
	attempts map[string][]*jobs.Attempt
	// steps is the per-job map of completed [jobs.StepRecord]s,
	// keyed by step name. Only one record per (jobID, name).
	steps map[string]map[string]*jobs.StepRecord
	// workers holds alive worker rows, keyed by worker id.
	workers map[string]*jobs.WorkerRow
	// schedules holds cron-style schedules keyed by Name.
	schedules map[string]*jobs.ScheduleRow
}

// New constructs an empty memory store.
func New() *Store {
	return &Store{
		rows:        make(map[string]*jobs.JobRow),
		uniqueIndex: make(map[string]string),
		attempts:    make(map[string][]*jobs.Attempt),
		steps:       make(map[string]map[string]*jobs.StepRecord),
		workers:     make(map[string]*jobs.WorkerRow),
		schedules:   make(map[string]*jobs.ScheduleRow),
	}
}

func (s *Store) Insert(_ context.Context, row *jobs.JobRow) error {
	if row == nil {
		return jobs.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rows[row.ID]; exists {
		return &jobs.DuplicateError{ExistingID: row.ID, Kind: row.Kind}
	}
	if row.UniqueKey != "" && !row.State.Terminal() {
		key := uniqKey(row.Kind, row.UniqueKey)
		if existingID, ok := s.uniqueIndex[key]; ok {
			if existing, ok := s.rows[existingID]; ok && !existing.State.Terminal() {
				return &jobs.DuplicateError{
					ExistingID: existingID,
					Kind:       row.Kind,
					UniqueKey:  row.UniqueKey,
				}
			}
		}
		s.uniqueIndex[key] = row.ID
	}
	cp := *row
	s.rows[row.ID] = &cp
	return nil
}

func (s *Store) InsertTx(_ context.Context, _ any, _ *jobs.JobRow) error {
	return jobs.ErrUnsupported
}

func (s *Store) Get(_ context.Context, id string) (*jobs.JobRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[id]
	if !ok {
		return nil, jobs.ErrNotFound
	}
	cp := *row
	return &cp, nil
}

func (s *Store) List(_ context.Context, f jobs.JobFilter) ([]*jobs.JobRow, string, error) {
	cursorTime, cursorID, err := jobs.DecodeJobsCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := jobs.NormalizeJobsLimit(f.Limit)

	s.mu.RLock()
	defer s.mu.RUnlock()

	matches := make([]*jobs.JobRow, 0, len(s.rows))
	for _, r := range s.rows {
		if !rowMatches(r, f) {
			continue
		}
		if f.Cursor != "" {
			if r.CreatedAt.Before(cursorTime) {
				continue
			}
			if r.CreatedAt.Equal(cursorTime) && r.ID <= cursorID {
				continue
			}
		}
		matches = append(matches, r)
	}
	slices.SortFunc(matches, func(a, b *jobs.JobRow) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	var next string
	if len(matches) > limit {
		last := matches[limit-1]
		next = jobs.EncodeJobsCursor(last.CreatedAt, last.ID)
		matches = matches[:limit]
	}

	out := make([]*jobs.JobRow, len(matches))
	for i, r := range matches {
		cp := *r
		out[i] = &cp
	}
	return out, next, nil
}

// Claim locks up to req.Limit eligible jobs and returns them as
// running. Eligibility: state in (available, scheduled), AvailableAt
// <= req.Now, queue matches one of req.Queues. Sort order: priority
// desc, then available_at asc. Honors req.QueueLimits (per-queue
// cap on this batch) and req.KindLimits (global running cap per
// kind).
func (s *Store) Claim(_ context.Context, req jobs.ClaimRequest) ([]*jobs.JobRow, error) {
	if req.WorkerID == "" {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}
	queues := make(map[string]struct{}, len(req.Queues))
	for _, q := range req.Queues {
		queues[q] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Count currently-running per kind so the per-kind cap can
	// reject candidates that would push the global count over the
	// limit.
	kindRunning := map[string]int{}
	if len(req.KindLimits) > 0 {
		for _, r := range s.rows {
			if r.State == jobs.StateRunning {
				kindRunning[r.Kind]++
			}
		}
	}

	candidates := make([]*jobs.JobRow, 0)
	for _, r := range s.rows {
		if r.State != jobs.StateAvailable && r.State != jobs.StateScheduled {
			continue
		}
		if !r.AvailableAt.IsZero() && r.AvailableAt.After(req.Now) {
			continue
		}
		if len(queues) > 0 {
			if _, ok := queues[r.Queue]; !ok {
				continue
			}
		}
		candidates = append(candidates, r)
	}
	slices.SortFunc(candidates, func(a, b *jobs.JobRow) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		return a.AvailableAt.Compare(b.AvailableAt)
	})

	now := req.Now
	until := now.Add(req.LeaseDuration)
	taken := make([]*jobs.JobRow, 0, limit)
	perQueueTaken := map[string]int{}

	for _, r := range candidates {
		if len(taken) >= limit {
			break
		}
		if qLimit, ok := req.QueueLimits[r.Queue]; ok && qLimit >= 0 {
			if perQueueTaken[r.Queue] >= qLimit {
				continue
			}
		}
		if kLimit, ok := req.KindLimits[r.Kind]; ok {
			if kindRunning[r.Kind] >= kLimit {
				continue
			}
		}
		r.State = jobs.StateRunning
		r.LockedBy = req.WorkerID
		r.LockedUntil = until
		r.HeartbeatAt = now
		r.UpdatedAt = now
		taken = append(taken, r)
		perQueueTaken[r.Queue]++
		kindRunning[r.Kind]++
	}

	out := make([]*jobs.JobRow, len(taken))
	for i, r := range taken {
		cp := *r
		out[i] = &cp
	}
	return out, nil
}

// Heartbeat extends the lease and reports whether cancellation was
// requested. Returns ErrNotFound when the job no longer exists OR
// was reclaimed by another worker (lease stolen).
func (s *Store) Heartbeat(_ context.Context, jobID, workerID string, until time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return false, jobs.ErrNotFound
	}
	if r.State != jobs.StateRunning || r.LockedBy != workerID {
		// Lease was lost to a sweep.
		return false, jobs.ErrNotFound
	}
	now := time.Now().UTC()
	r.LockedUntil = until
	r.HeartbeatAt = now
	r.UpdatedAt = now
	return r.CancelRequested, nil
}

// Complete writes the final outcome of an attempt. Rejects with
// ErrNotFound if the runner no longer holds the lease (so a stale
// completion from a stolen lease does not overwrite the new owner).
//
// When the row's CancelRequested flag is set and the runner is
// reporting StateSucceeded, the store overrides to StateCancelled:
// the user asked for cancellation and the worker observed it via
// the row even though Run completed before any heartbeat ticked.
// The returned State is the one actually written.
func (s *Store) Complete(_ context.Context, jobID, workerID string, o jobs.Outcome) (jobs.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return "", jobs.ErrNotFound
	}
	// Strict ownership: any deviation from "we still hold the lease"
	// is ErrNotFound, including state != running (sweep reclaimed
	// the row while this worker's process was paused). A permissive
	// check would let a slow worker's outcome clobber the sweep's
	// reclaim, corrupting the attempt count and the available_at.
	if r.State != jobs.StateRunning || r.LockedBy != workerID {
		return "", jobs.ErrNotFound
	}

	applied := o.State
	if o.State == jobs.StateSucceeded && r.CancelRequested {
		applied = jobs.StateCancelled
	}

	if o.FinalProgress != nil {
		r.ProgressDone = o.FinalProgress.Done
		r.ProgressTotal = o.FinalProgress.Total
		r.ProgressMsg = o.FinalProgress.Msg
	}
	r.State = applied
	r.Attempt = o.Attempt
	r.Error = o.Error
	if applied == jobs.StateScheduled {
		r.AvailableAt = o.AvailableAt
	}
	r.LockedBy = ""
	r.LockedUntil = time.Time{}
	r.HeartbeatAt = time.Time{}
	r.CancelRequested = false
	r.UpdatedAt = time.Now().UTC()

	// Free the unique-index slot if the row is now terminal so a
	// fresh enqueue of the same key can take it over.
	if r.UniqueKey != "" && r.State.Terminal() {
		key := uniqKey(r.Kind, r.UniqueKey)
		if existing, ok := s.uniqueIndex[key]; ok && existing == r.ID {
			delete(s.uniqueIndex, key)
		}
	}
	return applied, nil
}

// SweepExpired returns running jobs whose LockedUntil is in the
// past to the available pool, so another worker can claim them.
// For each reclaimed job, a synthetic Attempt row is appended to
// the ledger (state=failed, error="lease expired") attributed to
// the previous locked_by, and the row's Attempt counter is
// incremented so the next run is correctly numbered. Returns the
// number reclaimed.
func (s *Store) SweepExpired(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, r := range s.rows {
		if r.State != jobs.StateRunning {
			continue
		}
		if r.LockedUntil.IsZero() || r.LockedUntil.After(now) {
			continue
		}
		// The crashed attempt was r.Attempt + 1 (run() always uses
		// row.Attempt + 1 as the attemptNum it records). Append a
		// ledger row for it before resetting the state. When the
		// row carries an unobserved CancelRequested, label the
		// synthetic attempt as cancelled so the ledger reflects
		// the user's intent (the lease expired while they were
		// trying to cancel).
		attemptNum := r.Attempt + 1
		startedAt := r.HeartbeatAt
		if startedAt.IsZero() {
			startedAt = r.LockedUntil // best-effort lower bound
		}
		attemptState := jobs.AttemptFailed
		attemptError := "lease expired"
		if r.CancelRequested {
			attemptState = jobs.AttemptCancelled
			attemptError = "lease expired during cancellation"
		}
		s.attempts[r.ID] = append(s.attempts[r.ID], &jobs.Attempt{
			ID:         jobs.NewID(),
			JobID:      r.ID,
			Attempt:    attemptNum,
			WorkerID:   r.LockedBy,
			StartedAt:  startedAt,
			FinishedAt: now,
			State:      attemptState,
			Error:      attemptError,
		})
		r.Attempt = attemptNum
		r.State = jobs.StateAvailable
		r.LockedBy = ""
		r.LockedUntil = time.Time{}
		r.HeartbeatAt = time.Time{}
		r.UpdatedAt = now
		count++
	}
	return count, nil
}

func (s *Store) RecordAttempt(_ context.Context, a *jobs.Attempt) error {
	if a == nil || a.JobID == "" {
		return jobs.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *a
	s.attempts[a.JobID] = append(s.attempts[a.JobID], &cp)
	return nil
}

func (s *Store) ListAttempts(_ context.Context, jobID string, afterAttempt, limit int) ([]*jobs.Attempt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := s.attempts[jobID]
	out := make([]*jobs.Attempt, 0, len(rows))
	for _, r := range rows {
		if r.Attempt <= afterAttempt {
			continue
		}
		cp := *r
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) GetStep(_ context.Context, jobID, name string) (*jobs.StepRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byName, ok := s.steps[jobID]
	if !ok {
		return nil, jobs.ErrNotFound
	}
	rec, ok := byName[name]
	if !ok {
		return nil, jobs.ErrNotFound
	}
	cp := *rec
	return &cp, nil
}

func (s *Store) SaveStep(_ context.Context, r *jobs.StepRecord) error {
	if r == nil || r.JobID == "" || r.Name == "" {
		return jobs.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.steps[r.JobID]; !exists {
		s.steps[r.JobID] = make(map[string]*jobs.StepRecord)
	}
	if _, exists := s.steps[r.JobID][r.Name]; exists {
		// Mirror SQL backends' (job_id, name) uniqueness. The
		// runner only ever calls SaveStep after GetStep returned
		// ErrNotFound, so this fires only on a real race between
		// two workers persisting the same step.
		return fmt.Errorf("step %q on job %s already persisted", r.Name, r.JobID)
	}
	cp := *r
	s.steps[r.JobID][r.Name] = &cp
	return nil
}

func (s *Store) ListSteps(_ context.Context, jobID string) ([]*jobs.StepRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byName, ok := s.steps[jobID]
	if !ok {
		return nil, nil
	}
	out := make([]*jobs.StepRecord, 0, len(byName))
	for _, r := range byName {
		cp := *r
		out = append(out, &cp)
	}
	slices.SortFunc(out, func(a, b *jobs.StepRecord) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return out, nil
}

func (s *Store) UpdateProgress(_ context.Context, jobID string, done, total int64, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return jobs.ErrNotFound
	}
	r.ProgressDone = done
	r.ProgressTotal = total
	r.ProgressMsg = msg
	r.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Store) Retry(_ context.Context, jobID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return jobs.ErrNotFound
	}
	if !r.State.Terminal() {
		return jobs.ErrJobNotRetryable
	}
	// Preserve Attempt for history; bump max_attempts so one more
	// run is permitted regardless of where we are.
	if r.Attempt >= r.MaxAttempts {
		r.MaxAttempts = r.Attempt + 1
	}
	r.State = jobs.StateAvailable
	r.AvailableAt = now
	r.Error = ""
	r.LockedBy = ""
	r.LockedUntil = time.Time{}
	r.HeartbeatAt = time.Time{}
	r.CancelRequested = false
	r.UpdatedAt = now
	// Restore the unique-index entry if the row had one and the
	// previous occupant was cleared on its terminal transition.
	if r.UniqueKey != "" {
		s.uniqueIndex[uniqKey(r.Kind, r.UniqueKey)] = r.ID
	}
	return nil
}

func (s *Store) Cancel(_ context.Context, jobID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return false, jobs.ErrNotFound
	}
	if r.State.Terminal() {
		return false, jobs.ErrJobTerminal
	}
	if r.State == jobs.StateRunning {
		r.CancelRequested = true
		r.UpdatedAt = now
		return false, nil
	}
	// Scheduled or available: flip to cancelled immediately.
	r.State = jobs.StateCancelled
	r.UpdatedAt = now
	if r.UniqueKey != "" {
		key := uniqKey(r.Kind, r.UniqueKey)
		if existing, ok := s.uniqueIndex[key]; ok && existing == r.ID {
			delete(s.uniqueIndex, key)
		}
	}
	return true, nil
}

func (s *Store) Delete(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return jobs.ErrNotFound
	}
	if r.State == jobs.StateRunning {
		return jobs.ErrJobRunning
	}
	delete(s.rows, jobID)
	delete(s.attempts, jobID)
	delete(s.steps, jobID)
	if r.UniqueKey != "" {
		key := uniqKey(r.Kind, r.UniqueKey)
		if existing, ok := s.uniqueIndex[key]; ok && existing == r.ID {
			delete(s.uniqueIndex, key)
		}
	}
	return nil
}

func (s *Store) UpsertWorker(_ context.Context, w *jobs.WorkerRow) error {
	if w == nil || w.ID == "" {
		return jobs.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *w
	cp.Queues = append([]string(nil), w.Queues...)
	// Preserve the original StartedAt on update; only LastSeenAt
	// and the other fields move forward.
	if prev, ok := s.workers[w.ID]; ok {
		cp.StartedAt = prev.StartedAt
	}
	s.workers[w.ID] = &cp
	return nil
}

func (s *Store) RetireWorker(_ context.Context, workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workers, workerID)
	return nil
}

func (s *Store) SweepStaleWorkers(_ context.Context, olderThan time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, w := range s.workers {
		if !w.LastSeenAt.IsZero() && w.LastSeenAt.Before(olderThan) {
			delete(s.workers, id)
			removed++
		}
	}
	return removed, nil
}

func (s *Store) ListWorkers(_ context.Context) ([]*jobs.WorkerRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*jobs.WorkerRow, 0, len(s.workers))
	for _, w := range s.workers {
		cp := *w
		cp.Queues = append([]string(nil), w.Queues...)
		out = append(out, &cp)
	}
	slices.SortFunc(out, func(a, b *jobs.WorkerRow) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return out, nil
}

func (s *Store) ListQueues(_ context.Context) ([]jobs.QueueInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byName := map[string]map[jobs.State]int{}
	for _, r := range s.rows {
		byState, ok := byName[r.Queue]
		if !ok {
			byState = map[jobs.State]int{}
			byName[r.Queue] = byState
		}
		byState[r.State]++
	}
	out := make([]jobs.QueueInfo, 0, len(byName))
	for name, counts := range byName {
		// Defensive copy of the counts map.
		cp := make(map[jobs.State]int, len(counts))
		for k, v := range counts {
			cp[k] = v
		}
		out = append(out, jobs.QueueInfo{Name: name, Counts: cp})
	}
	slices.SortFunc(out, func(a, b jobs.QueueInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (s *Store) UpsertSchedule(_ context.Context, sched *jobs.ScheduleRow) error {
	if sched == nil || sched.Name == "" {
		return jobs.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sched
	cp.Payload = append([]byte(nil), sched.Payload...)
	cp.OptionsJSON = append([]byte(nil), sched.OptionsJSON...)
	// Preserve NextRunAt on upsert when the cron expression did not
	// change, so re-calling Schedule on every boot does not silently
	// swallow missed ticks (CatchUp policies break otherwise). When
	// the cron changes, the new NextRunAt applies.
	if prev, ok := s.schedules[sched.Name]; ok && prev.Cron == sched.Cron {
		cp.NextRunAt = prev.NextRunAt
	}
	s.schedules[sched.Name] = &cp
	return nil
}

func (s *Store) DeleteSchedule(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.schedules, name)
	return nil
}

func (s *Store) ListSchedules(_ context.Context) ([]*jobs.ScheduleRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*jobs.ScheduleRow, 0, len(s.schedules))
	for _, sched := range s.schedules {
		cp := *sched
		cp.Payload = append([]byte(nil), sched.Payload...)
		cp.OptionsJSON = append([]byte(nil), sched.OptionsJSON...)
		out = append(out, &cp)
	}
	slices.SortFunc(out, func(a, b *jobs.ScheduleRow) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (s *Store) DueSchedules(_ context.Context, now time.Time) ([]*jobs.ScheduleRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*jobs.ScheduleRow, 0)
	for _, sched := range s.schedules {
		if sched.NextRunAt.IsZero() {
			continue
		}
		if sched.NextRunAt.After(now) {
			continue
		}
		cp := *sched
		cp.Payload = append([]byte(nil), sched.Payload...)
		cp.OptionsJSON = append([]byte(nil), sched.OptionsJSON...)
		out = append(out, &cp)
	}
	return out, nil
}

func (s *Store) ClaimSchedule(_ context.Context, name string, expectedLast, newLast, newNext time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[name]
	if !ok {
		return false, jobs.ErrNotFound
	}
	if !sched.LastRunAt.Equal(expectedLast) {
		return false, nil
	}
	sched.LastRunAt = newLast
	sched.NextRunAt = newNext
	sched.UpdatedAt = newLast
	return true, nil
}

// --- helpers ---

func uniqKey(kind, key string) string { return kind + "\x00" + key }

func rowMatches(r *jobs.JobRow, f jobs.JobFilter) bool {
	if len(f.Queues) > 0 && !contains(f.Queues, r.Queue) {
		return false
	}
	if len(f.Kinds) > 0 && !contains(f.Kinds, r.Kind) {
		return false
	}
	if len(f.States) > 0 && !containsState(f.States, r.State) {
		return false
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func containsState(haystack []jobs.State, needle jobs.State) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
