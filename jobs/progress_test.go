package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// --- counting store ---
//
// Wraps a memory store, counts UpdateProgress calls. Used to assert
// that the runtime coalesces many Progress calls into few writes.
type countingStore struct {
	*memory.Store
	updateCalls atomic.Int32
}

func newCountingStore() *countingStore {
	return &countingStore{Store: memory.New()}
}

func (c *countingStore) UpdateProgress(ctx context.Context, jobID string, done, total int64, msg string) error {
	c.updateCalls.Add(1)
	return c.Store.UpdateProgress(ctx, jobID, done, total, msg)
}

// --- chattyJob: tightly loops Progress ---

type chattyJob struct {
	Tag string `json:"tag"`
}

type chattyControl struct {
	calls int
	gate  chan struct{}
}

var chattyCtrls = map[string]*chattyControl{}

func registerChatty(tag string, c *chattyControl) {
	chattyCtrls[tag] = c
}

func (j *chattyJob) Run(ctx jobs.Context) error {
	c := chattyCtrls[j.Tag]
	for i := 0; i < c.calls; i++ {
		ctx.Progress(int64(i+1), int64(c.calls), "tick")
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// --- tests ---

func TestProgress_CoalescesManyCallsIntoFewWrites(t *testing.T) {
	// 200 Progress calls inside a fast Run should produce at most a
	// couple of store writes (in practice: just the final flush
	// from Complete, since the first throttle tick is 500ms away).
	chattyCtrls = map[string]*chattyControl{}
	registerChatty("burst", &chattyControl{calls: 200})

	store := newCountingStore()
	m, _ := jobs.New(store, jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[chattyJob](m, "chatty"))
	id := enq(t, m, &chattyJob{Tag: "burst"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()
	waitForState(t, m, id, jobs.StateSucceeded, 2*time.Second)

	writes := store.updateCalls.Load()
	if writes > 3 {
		t.Errorf("UpdateProgress called %d times for 200 Progress calls, want <=3 (throttle)", writes)
	}

	// And the final value must be persisted: 200/200.
	info, _ := m.GetJob(context.Background(), id)
	if info.Progress.Done != 200 || info.Progress.Total != 200 {
		t.Errorf("final progress = %+v, want done=200 total=200", info.Progress)
	}
}

func TestProgress_VisibleMidJobAfterThrottleTick(t *testing.T) {
	// A long-running job that sets progress and then gates should
	// become observable to GetJob after one throttle tick (500ms).
	chattyCtrls = map[string]*chattyControl{}
	gate := make(chan struct{})
	registerChatty("hold", &chattyControl{calls: 1, gate: gate})

	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[chattyJob](m, "chatty"))
	id := enq(t, m, &chattyJob{Tag: "hold"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		close(gate)
		w.Stop(context.Background())
		<-done
	}()

	// Wait for the throttle tick to flush. 700ms = 500ms throttle +
	// 200ms slack for claim, lock acquisition, and goroutine
	// scheduling.
	time.Sleep(700 * time.Millisecond)

	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != jobs.StateRunning {
		t.Fatalf("State = %s, want running (gate is still closed)", info.State)
	}
	if info.Progress.Done != 1 || info.Progress.Total != 1 || info.Progress.Msg != "tick" {
		t.Errorf("mid-job progress = %+v, want done=1 total=1 msg=tick", info.Progress)
	}
}
