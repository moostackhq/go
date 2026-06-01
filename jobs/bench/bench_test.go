package bench_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
	"github.com/moostackhq/go/jobs/store/postgres"
	"github.com/moostackhq/go/jobs/store/sqlite"
)

// workerCounts is the matrix dimension. Add 16/32 if your numbers
// haven't plateaued.
var workerCounts = []int{1, 2, 4, 8}

// noopBenchJob is the smallest possible payload: one int field.
// Each Run is a no-op; the test measures pipeline overhead, not
// the user's handler.
type noopBenchJob struct {
	N int64 `json:"n"`
}

func (j *noopBenchJob) Run(_ jobs.Context) error { return nil }

// backendFactory builds a fresh [jobs.Store] for one benchmark run.
// build is called from b.Run so each sub-benchmark gets isolated
// state (e.g. its own SQLite file in t.TempDir()).
type backendFactory struct {
	name  string
	build func(tb testing.TB) (jobs.Store, bool) // returns store, ok; ok=false skips
}

func backends() []backendFactory {
	return []backendFactory{
		{
			name: "memory",
			build: func(_ testing.TB) (jobs.Store, bool) {
				return memory.New(), true
			},
		},
		{
			name: "sqlite",
			build: func(tb testing.TB) (jobs.Store, bool) {
				tb.Helper()
				db, err := sqlite.Open(filepath.Join(tb.TempDir(), "bench.db"))
				if err != nil {
					tb.Fatal(err)
				}
				tb.Cleanup(func() { _ = db.Close() })
				s, err := sqlite.New(db, sqlite.Options{AutoCreate: true})
				if err != nil {
					tb.Fatal(err)
				}
				return s, true
			},
		},
		{
			name: "postgres",
			build: func(tb testing.TB) (jobs.Store, bool) {
				tb.Helper()
				dsn := os.Getenv("JOBS_PG_URL")
				if dsn == "" {
					return nil, false // skip
				}
				db, err := sql.Open("postgres", dsn)
				if err != nil {
					tb.Fatal(err)
				}
				if err := db.PingContext(context.Background()); err != nil {
					tb.Skipf("cannot connect to %s: %v", dsn, err)
					return nil, false
				}
				tb.Cleanup(func() { _ = db.Close() })
				s, err := postgres.New(db, postgres.Options{AutoCreate: true})
				if err != nil {
					tb.Fatal(err)
				}
				// Wipe so previous runs don't pollute the numbers.
				for _, table := range []string{"job_attempts", "job_steps", "schedules", "workers", "jobs"} {
					if _, err := db.ExecContext(context.Background(), "DELETE FROM "+table); err != nil {
						tb.Fatalf("reset %s: %v", table, err)
					}
				}
				return s, true
			},
		},
	}
}

// BenchmarkEnqueue measures pure enqueue throughput per backend
// with no workers running. This is the upper bound on how fast the
// caller (e.g. an HTTP handler) can push work into the system.
func BenchmarkEnqueue(b *testing.B) {
	for _, be := range backends() {
		b.Run(be.name, func(b *testing.B) {
			store, ok := be.build(b)
			if !ok {
				b.Skipf("%s backend not available (set JOBS_PG_URL)", be.name)
			}
			runEnqueue(b, store)
		})
	}
}

// BenchmarkPipeline measures end-to-end throughput per backend at
// varying worker counts. Each worker has Concurrency=1, so N workers
// means N parallel claim/run/complete pipelines competing for the
// same store. ns/op divides total benchmark time by drained jobs;
// the appended jobs/sec metric is the inverse and is the headline
// number for cross-backend comparison.
//
// Each sub-benchmark builds a fresh store so succeeded-job rows
// from one configuration do not slow down the next (relevant for
// the memory backend, which iterates all rows during Claim).
func BenchmarkPipeline(b *testing.B) {
	for _, be := range backends() {
		for _, n := range workerCounts {
			b.Run(fmt.Sprintf("%s/workers=%d", be.name, n), func(b *testing.B) {
				store, ok := be.build(b)
				if !ok {
					b.Skipf("%s backend not available (set JOBS_PG_URL)", be.name)
				}
				runPipeline(b, store, n)
			})
		}
	}
}

func runEnqueue(b *testing.B, store jobs.Store) {
	m, err := jobs.New(store, jobs.Config{})
	if err != nil {
		b.Fatal(err)
	}
	if err := jobs.Register[noopBenchJob](m, "noop"); err != nil {
		b.Fatal(err)
	}
	job := &noopBenchJob{N: 1}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Enqueue(ctx, job); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "jobs/sec")
}

func runPipeline(b *testing.B, store jobs.Store, workers int) {
	var completed atomic.Int64
	m, err := jobs.New(store, jobs.Config{
		Hooks: jobs.Hooks{
			OnFinish: func(_ context.Context, _ jobs.FinishEvent) {
				completed.Add(1)
			},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := jobs.Register[noopBenchJob](m, "noop"); err != nil {
		b.Fatal(err)
	}

	cfg := jobs.WorkerConfig{
		Concurrency:       1,
		PollInterval:      1 * time.Millisecond,
		LeaseDuration:     5 * time.Second,
		HeartbeatInterval: 500 * time.Millisecond,
		SweepInterval:     5 * time.Second,
	}
	ws := make([]*jobs.Worker, workers)
	dones := make([]chan struct{}, workers)
	for i := range ws {
		w, err := jobs.NewWorker(m, cfg)
		if err != nil {
			b.Fatal(err)
		}
		ws[i] = w
		done := make(chan struct{})
		dones[i] = done
		go func() {
			defer close(done)
			_ = w.Start(context.Background())
		}()
	}
	defer func() {
		for _, w := range ws {
			w.Stop(context.Background())
		}
		for _, d := range dones {
			<-d
		}
	}()

	// Let the workers register and prime their pollers.
	time.Sleep(20 * time.Millisecond)

	job := &noopBenchJob{N: 1}
	ctx := context.Background()
	completed.Store(0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Enqueue(ctx, job); err != nil {
			b.Fatal(err)
		}
	}

	deadline := time.Now().Add(180 * time.Second)
	for completed.Load() < int64(b.N) {
		if time.Now().After(deadline) {
			b.Fatalf("drain timeout: completed=%d want=%d", completed.Load(), b.N)
		}
		time.Sleep(500 * time.Microsecond)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "jobs/sec")
}
