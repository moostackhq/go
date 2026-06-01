package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// concurrencyJob tracks the high-water mark of simultaneous Runs.
// Tests assert this never exceeds the configured limit.
type concurrencyJob struct {
	Tag string `json:"tag"`
}

type concurrencyControl struct {
	current   atomic.Int32
	maxSeen   atomic.Int32
	holdFor   time.Duration
	runStarts chan struct{} // signals each Run start; tests read to know jobs are in-flight
}

var concurrencyCtrls = map[string]*concurrencyControl{}

func registerConcurrency(tag string, c *concurrencyControl) {
	concurrencyCtrls[tag] = c
}

func (j *concurrencyJob) Run(ctx jobs.Context) error {
	c := concurrencyCtrls[j.Tag]
	cur := c.current.Add(1)
	for {
		seen := c.maxSeen.Load()
		if cur <= seen || c.maxSeen.CompareAndSwap(seen, cur) {
			break
		}
	}
	if c.runStarts != nil {
		select {
		case c.runStarts <- struct{}{}:
		default:
		}
	}
	defer c.current.Add(-1)
	select {
	case <-time.After(c.holdFor):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func TestConcurrency_PerWorkerCapNeverExceeded(t *testing.T) {
	concurrencyCtrls = map[string]*concurrencyControl{}
	ctl := &concurrencyControl{
		holdFor:   80 * time.Millisecond,
		runStarts: make(chan struct{}, 100),
	}
	registerConcurrency("worker-cap", ctl)

	m, _ := jobs.New(memory.New(), jobs.Config{})
	must(t, jobs.Register[concurrencyJob](m, "concurrency"))
	for i := 0; i < 10; i++ {
		enq(t, m, &concurrencyJob{Tag: "worker-cap"}, jobs.Options{})
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		Concurrency:       3,
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     500 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		SweepInterval:     100 * time.Millisecond,
	})
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page, _ := m.ListJobs(context.Background(), jobs.JobFilter{
			States: []jobs.State{jobs.StateSucceeded},
		})
		if len(page.Jobs) == 10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := ctl.maxSeen.Load(); got > 3 {
		t.Errorf("max simultaneous = %d, want <= 3 (worker concurrency cap)", got)
	}
	if got := ctl.maxSeen.Load(); got < 2 {
		t.Errorf("max simultaneous = %d, want >= 2 (parallelism actually happened)", got)
	}
}

func TestConcurrency_PerQueueCap(t *testing.T) {
	// Two queues, worker concurrency 5, but PerQueue caps each at 1.
	// Expectation: simultaneous count for each queue never exceeds 1,
	// but across queues we should still see two jobs running at once.
	concurrencyCtrls = map[string]*concurrencyControl{}
	emailCtl := &concurrencyControl{holdFor: 80 * time.Millisecond}
	reportCtl := &concurrencyControl{holdFor: 80 * time.Millisecond}
	registerConcurrency("q-emails", emailCtl)
	registerConcurrency("q-reports", reportCtl)

	m, _ := jobs.New(memory.New(), jobs.Config{})
	must(t, jobs.Register[concurrencyJob](m, "concurrency"))
	for i := 0; i < 5; i++ {
		enq(t, m, &concurrencyJob{Tag: "q-emails"}, jobs.Options{Queue: "emails"})
		enq(t, m, &concurrencyJob{Tag: "q-reports"}, jobs.Options{Queue: "reports"})
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		Queues:            []string{"emails", "reports"},
		Concurrency:       5,
		PerQueue:          map[string]int{"emails": 1, "reports": 1},
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     500 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		SweepInterval:     100 * time.Millisecond,
	})
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page, _ := m.ListJobs(context.Background(), jobs.JobFilter{
			States: []jobs.State{jobs.StateSucceeded},
		})
		if len(page.Jobs) == 10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := emailCtl.maxSeen.Load(); got > 1 {
		t.Errorf("emails queue max simultaneous = %d, want <= 1", got)
	}
	if got := reportCtl.maxSeen.Load(); got > 1 {
		t.Errorf("reports queue max simultaneous = %d, want <= 1", got)
	}
}

func TestConcurrency_KindLimit_GlobalAcrossWorkers(t *testing.T) {
	// SetKindLimit caps "concurrency" kind to 1 across two workers
	// drinking from the same queue.
	concurrencyCtrls = map[string]*concurrencyControl{}
	ctl := &concurrencyControl{holdFor: 60 * time.Millisecond}
	registerConcurrency("global", ctl)

	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})
	must(t, jobs.Register[concurrencyJob](m, "concurrency"))
	m.SetKindLimit("concurrency", 1)

	for i := 0; i < 6; i++ {
		enq(t, m, &concurrencyJob{Tag: "global"}, jobs.Options{})
	}

	mkWorker := func() *jobs.Worker {
		w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
			Concurrency:       3,
			PollInterval:      5 * time.Millisecond,
			LeaseDuration:     500 * time.Millisecond,
			HeartbeatInterval: 100 * time.Millisecond,
			SweepInterval:     100 * time.Millisecond,
		})
		return w
	}
	w1, w2 := mkWorker(), mkWorker()
	d1, d2 := startWorker(t, w1), startWorker(t, w2)
	defer func() {
		w1.Stop(context.Background())
		w2.Stop(context.Background())
		<-d1
		<-d2
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page, _ := m.ListJobs(context.Background(), jobs.JobFilter{
			States: []jobs.State{jobs.StateSucceeded},
		})
		if len(page.Jobs) == 6 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := ctl.maxSeen.Load(); got > 1 {
		t.Errorf("max simultaneous of kind = %d, want <= 1 (SetKindLimit)", got)
	}
}

func TestSetKindLimit_RemovedByZero(t *testing.T) {
	concurrencyCtrls = map[string]*concurrencyControl{}
	ctl := &concurrencyControl{holdFor: 40 * time.Millisecond}
	registerConcurrency("toggle", ctl)

	m, _ := jobs.New(memory.New(), jobs.Config{})
	must(t, jobs.Register[concurrencyJob](m, "concurrency"))
	m.SetKindLimit("concurrency", 1)
	m.SetKindLimit("concurrency", 0) // remove

	for i := 0; i < 5; i++ {
		enq(t, m, &concurrencyJob{Tag: "toggle"}, jobs.Options{})
	}

	w, _ := jobs.NewWorker(m, jobs.WorkerConfig{
		Concurrency:       3,
		PollInterval:      5 * time.Millisecond,
		LeaseDuration:     500 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		SweepInterval:     100 * time.Millisecond,
	})
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, _ := m.ListJobs(context.Background(), jobs.JobFilter{
			States: []jobs.State{jobs.StateSucceeded},
		})
		if len(page.Jobs) == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// With the limit removed, concurrency should reach the worker cap of 3.
	if got := ctl.maxSeen.Load(); got < 2 {
		t.Errorf("max simultaneous = %d, want >= 2 (limit removed)", got)
	}
}
