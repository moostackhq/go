// Command emails is a small HTTP service that enqueues SendEmail
// jobs and runs a worker in the same process. It exercises the
// library in realistic shape: register, enqueue, worker, durable
// step, scheduler, hooks, graceful shutdown.
//
// Run:
//
//	go run ./example/emails
//
// Enqueue an email:
//
//	curl -X POST localhost:8080/send -d '{"to":"a@b.com","subject":"hi","body":"world"}'
//
// Watch state:
//
//	curl localhost:8080/jobs
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// SendEmail is the job type. The Run method composes durable steps:
// build-message and deliver are individually retried on resume.
type SendEmail struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (j *SendEmail) Run(ctx jobs.Context) error {
	logger := ctx.Logger()

	msg, err := jobs.Step(ctx, "build-message", func(_ context.Context) (string, error) {
		// Real implementations would render templates, fetch user
		// preferences, etc. Anything expensive here is worth a step.
		return fmt.Sprintf("To: %s\nSubject: %s\n\n%s\n", j.To, j.Subject, j.Body), nil
	})
	if err != nil {
		return err
	}

	_, err = jobs.Step(ctx, "deliver", func(ctx context.Context) (any, error) {
		// Stand-in for an SMTP send. A real deliver might fail with
		// jobs.ErrPermanent on a bounce, or a regular error to be
		// retried with backoff.
		logger.Info("sending email",
			"to", j.To, "subject", j.Subject, "bytes", len(msg))
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		deliveries.Add(1)
		return nil, nil
	})
	return err
}

// DailyReport is the scheduled job: in real life it would assemble
// some metrics. Here it just logs.
type DailyReport struct{}

func (j *DailyReport) Run(ctx jobs.Context) error {
	ctx.Logger().Info("compiling daily report")
	return nil
}

var deliveries atomic.Int64

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	mgr, err := jobs.New(memory.New(), jobs.Config{
		Logger:         logger,
		DefaultBackoff: jobs.ExponentialBackoff{Base: 2 * time.Second, Max: 1 * time.Minute, Jitter: 0.2},
		Hooks: jobs.Hooks{
			OnFinish: func(_ context.Context, e jobs.FinishEvent) {
				if e.Err != nil {
					e.Logger.Error("job finished with error",
						"err", e.Err, "dur", e.Dur, "attempt", e.Attempt)
				} else {
					e.Logger.Info("job finished",
						"dur", e.Dur, "attempt", e.Attempt)
				}
			},
		},
	})
	if err != nil {
		logger.Error("create manager", "err", err)
		os.Exit(1)
	}

	if err := errors.Join(
		jobs.Register[SendEmail](mgr, "send_email"),
		jobs.Register[DailyReport](mgr, "daily_report"),
	); err != nil {
		logger.Error("register", "err", err)
		os.Exit(1)
	}

	// Daily report at 6 AM, singleton so a long-running prior run
	// blocks the next one.
	if err := mgr.Schedule(context.Background(), "daily-report", "0 6 * * *",
		&DailyReport{}, jobs.ScheduleOptions{Singleton: true}); err != nil {
		logger.Error("schedule", "err", err)
		os.Exit(1)
	}

	worker, err := jobs.NewWorker(mgr, jobs.WorkerConfig{
		Queues:      []string{"default"},
		Concurrency: 4,
	})
	if err != nil {
		logger.Error("new worker", "err", err)
		os.Exit(1)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if err := worker.Start(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("worker exited", "err", err)
		}
	}()

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		if err := mgr.StartScheduler(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("scheduler exited", "err", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var job SendEmail
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := mgr.Enqueue(r.Context(), &job)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		page, err := mgr.ListJobs(r.Context(), jobs.JobFilter{Limit: 50})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		queues, _ := mgr.ListQueues(r.Context())
		workers, _ := mgr.ListWorkers(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"queues":     queues,
			"workers":    workers,
			"deliveries": deliveries.Load(),
		})
	})

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		logger.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http", "err", err)
		}
	}()

	<-rootCtx.Done()
	logger.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	worker.Stop(shutCtx)
	<-workerDone
	<-schedDone
}
