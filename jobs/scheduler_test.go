package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// reportJob counts how many times it ran via a shared atomic.
type reportJob struct {
	Tag string `json:"tag"`
}

var reportRuns = map[string]*atomic.Int32{}

func (j *reportJob) Run(_ jobs.Context) error {
	if c := reportRuns[j.Tag]; c != nil {
		c.Add(1)
	}
	return nil
}

func resetReportJobs() {
	reportRuns = map[string]*atomic.Int32{}
}

func registerReport(tag string) *atomic.Int32 {
	c := &atomic.Int32{}
	reportRuns[tag] = c
	return c
}

func TestSchedule_AdvancesNextRunAtAndEnqueuesOnTick(t *testing.T) {
	resetReportJobs()
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{
		SchedulerInterval: 20 * time.Millisecond,
	})
	must(t, jobs.Register[reportJob](m, "report"))
	runs := registerReport("hourly")

	// Schedule for every hour, but stale the NextRunAt so the first
	// tick of the scheduler fires.
	if err := m.Schedule(context.Background(), "hourly", "0 * * * *",
		&reportJob{Tag: "hourly"}, jobs.ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background())
	rows[0].NextRunAt = time.Now().Add(-2 * time.Minute)
	rows[0].LastRunAt = time.Time{}
	_ = store.DeleteSchedule(context.Background(), rows[0].Name); _ = store.UpsertSchedule(context.Background(), rows[0])

	// Start a worker so the enqueued job runs.
	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	wDone := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-wDone
	}()

	schedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		_ = m.StartScheduler(schedCtx)
	}()

	// Wait for the job to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runs.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if runs.Load() == 0 {
		t.Fatal("scheduled job never ran")
	}

	rows2, _ := store.ListSchedules(context.Background())
	if !rows2[0].NextRunAt.After(time.Now()) {
		t.Errorf("NextRunAt = %v, want future", rows2[0].NextRunAt)
	}

	cancel()
	<-schedDone
}

func TestSchedule_TwoSchedulersFireOnceTotal(t *testing.T) {
	resetReportJobs()
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{
		SchedulerInterval: 20 * time.Millisecond,
	})
	must(t, jobs.Register[reportJob](m, "report"))
	runs := registerReport("once")

	if err := m.Schedule(context.Background(), "once", "0 * * * *",
		&reportJob{Tag: "once"}, jobs.ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background())
	rows[0].NextRunAt = time.Now().Add(-1 * time.Minute)
	rows[0].LastRunAt = time.Time{}
	_ = store.DeleteSchedule(context.Background(), rows[0].Name); _ = store.UpsertSchedule(context.Background(), rows[0])

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	wDone := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-wDone
	}()

	// Run two schedulers concurrently against the same manager.
	schedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done1, done2 := make(chan struct{}), make(chan struct{})
	go func() { defer close(done1); _ = m.StartScheduler(schedCtx) }()
	go func() { defer close(done2); _ = m.StartScheduler(schedCtx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runs.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Wait one more scheduler interval to make sure neither has
	// queued a second fire on the same overdue tick.
	time.Sleep(80 * time.Millisecond)

	if got := runs.Load(); got != 1 {
		t.Errorf("runs = %d, want 1 (two schedulers must dedupe a missed tick)", got)
	}

	cancel()
	<-done1
	<-done2
}

func TestSchedule_CatchUpAll_FiresMultiple(t *testing.T) {
	resetReportJobs()
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{
		SchedulerInterval: 20 * time.Millisecond,
	})
	must(t, jobs.Register[reportJob](m, "report"))
	runs := registerReport("all")

	// Every minute schedule; rewind NextRunAt 3 minutes 30 seconds
	// into the past so CatchUpAll fires 4 times (the 4 missed
	// minutes).
	if err := m.Schedule(context.Background(), "all", "* * * * *",
		&reportJob{Tag: "all"}, jobs.ScheduleOptions{
			CatchUp: jobs.CatchUpAll,
		}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background())
	rows[0].NextRunAt = time.Now().Add(-3*time.Minute - 30*time.Second)
	rows[0].LastRunAt = time.Time{}
	_ = store.DeleteSchedule(context.Background(), rows[0].Name); _ = store.UpsertSchedule(context.Background(), rows[0])

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	wDone := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-wDone
	}()

	schedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedDone := make(chan struct{})
	go func() { defer close(schedDone); _ = m.StartScheduler(schedCtx) }()

	// Wait for >=4 fires (3 full missed minutes + the current tick boundary).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runs.Load() < 4 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runs.Load(); got < 3 || got > 5 {
		t.Errorf("runs = %d, want 3-5 (CatchUpAll over ~4 missed minutes)", got)
	}

	cancel()
	<-schedDone
}

func TestSchedule_CatchUpSkip_FiresNone(t *testing.T) {
	resetReportJobs()
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{
		SchedulerInterval: 20 * time.Millisecond,
	})
	must(t, jobs.Register[reportJob](m, "report"))
	runs := registerReport("skip")

	if err := m.Schedule(context.Background(), "skip", "* * * * *",
		&reportJob{Tag: "skip"}, jobs.ScheduleOptions{
			CatchUp: jobs.CatchUpSkip,
		}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background())
	rows[0].NextRunAt = time.Now().Add(-10 * time.Minute)
	_ = store.DeleteSchedule(context.Background(), rows[0].Name); _ = store.UpsertSchedule(context.Background(), rows[0])

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	wDone := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-wDone
	}()

	schedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedDone := make(chan struct{})
	go func() { defer close(schedDone); _ = m.StartScheduler(schedCtx) }()

	// Wait for the scheduler to process at least one tick.
	time.Sleep(150 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Errorf("runs = %d, want 0 (CatchUpSkip)", got)
	}

	cancel()
	<-schedDone
}

func TestSchedule_ReSchedulePreservesMissedTicksForCatchUp(t *testing.T) {
	// The actual user-visible bug case: app crashes at 5:55, restarts
	// at 6:05, calls Schedule again with the same cron. CatchUpOnce
	// must still fire the missed 6 AM tick — re-Schedule must not
	// silently overwrite NextRunAt past the missed tick.
	resetReportJobs()
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{
		SchedulerInterval: 20 * time.Millisecond,
	})
	must(t, jobs.Register[reportJob](m, "report"))
	c := registerReport("preserved")

	// First boot: schedule and rewind NextRunAt into the past (the
	// "missed tick" the app was down for).
	must(t, m.Schedule(context.Background(), "preserved", "0 * * * *",
		&reportJob{Tag: "preserved"}, jobs.ScheduleOptions{CatchUp: jobs.CatchUpOnce}))
	rows, _ := store.ListSchedules(context.Background())
	rows[0].NextRunAt = time.Now().Add(-2 * time.Minute)
	rows[0].LastRunAt = time.Time{}
	_ = store.DeleteSchedule(context.Background(), rows[0].Name); _ = store.UpsertSchedule(context.Background(), rows[0])

	// Second "boot": same Schedule call. Before the fix this would
	// reset NextRunAt to parsed.Next(now) (a future tick) and the
	// missed one would be silently dropped.
	must(t, m.Schedule(context.Background(), "preserved", "0 * * * *",
		&reportJob{Tag: "preserved"}, jobs.ScheduleOptions{CatchUp: jobs.CatchUpOnce}))

	rows, _ = store.ListSchedules(context.Background())
	if rows[0].NextRunAt.After(time.Now()) {
		t.Fatalf("re-Schedule advanced NextRunAt past now (= %v); the missed tick would be dropped",
			rows[0].NextRunAt)
	}

	// Now start a worker + scheduler; the missed tick should fire.
	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	wDone := startWorker(t, w)
	defer func() { w.Stop(context.Background()); <-wDone }()

	schedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedDone := make(chan struct{})
	go func() { defer close(schedDone); _ = m.StartScheduler(schedCtx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && c.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-schedDone
	if c.Load() == 0 {
		t.Error("CatchUpOnce did not fire the missed tick after re-Schedule")
	}
}

func TestSchedule_UpsertNoDuplicates(t *testing.T) {
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})
	must(t, jobs.Register[reportJob](m, "report"))

	for i := 0; i < 3; i++ {
		if err := m.Schedule(context.Background(), "upsert", "0 6 * * *",
			&reportJob{Tag: "upsert"}, jobs.ScheduleOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := store.ListSchedules(context.Background())
	if len(rows) != 1 {
		t.Errorf("schedule rows = %d, want 1 (upsert)", len(rows))
	}
}

func TestUnschedule_RemovesRow(t *testing.T) {
	store := memory.New()
	m, _ := jobs.New(store, jobs.Config{})
	must(t, jobs.Register[reportJob](m, "report"))

	if err := m.Schedule(context.Background(), "byebye", "0 6 * * *",
		&reportJob{Tag: "byebye"}, jobs.ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Unschedule(context.Background(), "byebye"); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background())
	if len(rows) != 0 {
		t.Errorf("rows after Unschedule = %d, want 0", len(rows))
	}
}

