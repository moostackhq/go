package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// --- ErrPermanent ---

func TestWorker_ErrPermanent_FailsImmediately(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	ctl := &trackedJobControl{returnErr: fmt.Errorf("invalid: %w", jobs.ErrPermanent)}
	registerTrackedJob("perm", ctl)
	id := enq(t, m, &trackedJob{Tag: "perm"}, jobs.Options{
		MaxAttempts: 5, // would normally retry; ErrPermanent overrides
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateFailed, 1*time.Second)
	if r := ctl.runs.Load(); r != 1 {
		t.Errorf("runs = %d, want 1 (no retry on ErrPermanent)", r)
	}
	attempts := waitForAttempts(t, m, id, 1, 1*time.Second)
	if attempts[0].State != jobs.AttemptFailed {
		t.Errorf("attempt state = %q, want failed", attempts[0].State)
	}
}

// --- Timeout dispatch ---

func TestWorker_Timeout_RetryByDefault(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	// Deterministic: the first attempt blocks until its 30ms
	// timeout fires; the second attempt runs to completion.
	ctl := &trackedJobControl{blockUntilCtxOnFirstN: 1}
	registerTrackedJob("timeout-retry", ctl)
	id := enq(t, m, &trackedJob{Tag: "timeout-retry"}, jobs.Options{
		MaxAttempts: 3,
		Timeout:     30 * time.Millisecond,
		// OnTimeout zero value = TimeoutRetry
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateSucceeded, 2*time.Second)
	attempts := waitForAttempts(t, m, id, 2, 1*time.Second)
	if attempts[0].State != jobs.AttemptTimedOut {
		t.Errorf("first attempt state = %q, want timed_out", attempts[0].State)
	}
	if attempts[1].State != jobs.AttemptSucceeded {
		t.Errorf("second attempt state = %q, want succeeded", attempts[1].State)
	}
}

func TestWorker_Timeout_FailMode_GoesStraightToFailed(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{}) // never closes
	ctl := &trackedJobControl{gate: gate}
	registerTrackedJob("timeout-fail", ctl)
	id := enq(t, m, &trackedJob{Tag: "timeout-fail"}, jobs.Options{
		MaxAttempts: 5,
		Timeout:     30 * time.Millisecond,
		OnTimeout:   jobs.TimeoutFail,
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateFailed, 1*time.Second)
	if r := ctl.runs.Load(); r != 1 {
		t.Errorf("runs = %d, want 1 (no retry on TimeoutFail)", r)
	}
}

func TestWorker_Timeout_DiscardMode(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	gate := make(chan struct{}) // never closes
	ctl := &trackedJobControl{gate: gate}
	registerTrackedJob("timeout-discard", ctl)
	id := enq(t, m, &trackedJob{Tag: "timeout-discard"}, jobs.Options{
		MaxAttempts: 5,
		Timeout:     30 * time.Millisecond,
		OnTimeout:   jobs.TimeoutDiscard,
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateDiscarded, 1*time.Second)
}

// --- Attempts history ---

func TestListJobAttempts_RecordsAllAttempts(t *testing.T) {
	resetTrackedJobs()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond, Max: 5 * time.Millisecond},
	})
	must(t, jobs.Register[trackedJob](m, "tracked"))

	ctl := &trackedJobControl{returnErr: errors.New("nope")}
	registerTrackedJob("hist", ctl)
	id := enq(t, m, &trackedJob{Tag: "hist"}, jobs.Options{
		MaxAttempts: 3,
	})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateDiscarded, 2*time.Second)
	attempts := waitForAttempts(t, m, id, 3, 1*time.Second)
	for i, a := range attempts {
		want := i + 1
		if a.Attempt != want {
			t.Errorf("attempts[%d].Attempt = %d, want %d", i, a.Attempt, want)
		}
		if a.State != jobs.AttemptFailed {
			t.Errorf("attempts[%d].State = %q, want failed", i, a.State)
		}
		if a.StartedAt.IsZero() || a.FinishedAt.IsZero() {
			t.Errorf("attempts[%d] missing timestamps", i)
		}
		if a.WorkerID == "" {
			t.Errorf("attempts[%d] missing worker id", i)
		}
	}
}

func TestListJobAttempts_NoAttemptsYet(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[trackedJob](m, "tracked"))
	id := enq(t, m, &trackedJob{Tag: "fresh"}, jobs.Options{})

	page, err := m.ListJobAttempts(context.Background(), id, jobs.AttemptsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 0 {
		t.Errorf("attempts = %d, want 0 (job never ran)", len(page.Attempts))
	}
}

func TestAttemptCursor_EncodeDecodeRoundTrip(t *testing.T) {
	for _, n := range []int{1, 25, 1000, 999999} {
		c := jobs.EncodeAttemptsCursor(n)
		got, err := jobs.DecodeAttemptsCursor(c)
		if err != nil {
			t.Errorf("DecodeAttemptsCursor(%q): %v", c, err)
		}
		if got != n {
			t.Errorf("round trip = %d, want %d", got, n)
		}
	}
}

func TestAttemptCursor_DecodeEmpty(t *testing.T) {
	got, err := jobs.DecodeAttemptsCursor("")
	if err != nil || got != 0 {
		t.Errorf("empty cursor: got=%d err=%v, want 0/nil", got, err)
	}
}

func TestAttemptCursor_DecodeMalformed(t *testing.T) {
	if _, err := jobs.DecodeAttemptsCursor("not base64!"); err == nil {
		t.Error("expected error for non-base64 cursor")
	}
}

func TestListJobAttempts_PaginationRoundTrip(t *testing.T) {
	// Synthesise 12 attempts directly via the store, then walk
	// them with Limit=5 and verify every page boundary works and
	// every attempt appears exactly once in order.
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})

	jobID := "paginated-job"
	now := time.Now()
	for i := 1; i <= 12; i++ {
		if err := store.RecordAttempt(context.Background(), &jobs.Attempt{
			ID:         "att-" + strconv.Itoa(i),
			JobID:      jobID,
			Attempt:    i,
			WorkerID:   "w",
			StartedAt:  now.Add(time.Duration(i) * time.Millisecond),
			FinishedAt: now.Add(time.Duration(i)*time.Millisecond + time.Millisecond),
			State:      jobs.AttemptFailed,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var seen []int
	cursor := ""
	pages := 0
	for {
		page, err := m.ListJobAttempts(context.Background(), jobID, jobs.AttemptsFilter{
			Limit:  5,
			Cursor: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, a := range page.Attempts {
			seen = append(seen, a.Attempt)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 (12 / 5 = 2.4 -> 3 pages)", pages)
	}
	if len(seen) != 12 {
		t.Fatalf("seen %d attempts, want 12", len(seen))
	}
	for i, n := range seen {
		if n != i+1 {
			t.Errorf("seen[%d] = %d, want %d (ordered, no gaps, no dups)", i, n, i+1)
		}
	}
}
