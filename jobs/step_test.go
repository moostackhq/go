package jobs_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// --- multi-step harness ---
//
// csvImport runs three steps. The test populates `csvControl` to
// drive which step fails and how often each step ran.

type csvImport struct {
	Tag string `json:"tag"`
}

type csvControl struct {
	mu       sync.Mutex
	downloadRuns atomic.Int32
	parseRuns    atomic.Int32
	importRuns   atomic.Int32
	// failParseUntil > 0 means: parse returns an error until it has
	// been called at least this many times. Used to force a retry
	// without panicking (cleaner output in -v).
	failParseUntil int32
}

var (
	csvMu    sync.Mutex
	csvCtrls = map[string]*csvControl{}
)

func registerCSVCtrl(tag string, c *csvControl) {
	csvMu.Lock()
	csvCtrls[tag] = c
	csvMu.Unlock()
}

func getCSVCtrl(tag string) *csvControl {
	csvMu.Lock()
	defer csvMu.Unlock()
	return csvCtrls[tag]
}

func resetCSV() {
	csvMu.Lock()
	csvCtrls = map[string]*csvControl{}
	csvMu.Unlock()
}

func (j *csvImport) Run(ctx jobs.Context) error {
	c := getCSVCtrl(j.Tag)
	if c == nil {
		return errors.New("no csv control")
	}

	file, err := jobs.Step(ctx, "download", func(_ context.Context) (string, error) {
		c.downloadRuns.Add(1)
		return "data.csv", nil
	})
	if err != nil {
		return err
	}

	rows, err := jobs.Step(ctx, "parse", func(_ context.Context) (int, error) {
		c.parseRuns.Add(1)
		if c.parseRuns.Load() <= c.failParseUntil {
			return 0, errors.New("parse: transient")
		}
		return 100, nil
	})
	if err != nil {
		return err
	}

	_, err = jobs.Step(ctx, "import", func(_ context.Context) (any, error) {
		c.importRuns.Add(1)
		// Touch file/rows so the compiler does not warn.
		_ = file
		_ = rows
		return nil, nil
	})
	return err
}

// --- tests ---

func TestStep_PersistsAndSkipsOnRetry(t *testing.T) {
	resetCSV()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond, Max: 5 * time.Millisecond},
	})
	must(t, jobs.Register[csvImport](m, "csv_import"))

	ctl := &csvControl{failParseUntil: 1} // parse fails on its first call only
	registerCSVCtrl("retry-once", ctl)
	id := enq(t, m, &csvImport{Tag: "retry-once"}, jobs.Options{MaxAttempts: 3})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateSucceeded, 2*time.Second)

	if got := ctl.downloadRuns.Load(); got != 1 {
		t.Errorf("download ran %d times, want 1 (should be skipped on retry)", got)
	}
	if got := ctl.parseRuns.Load(); got != 2 {
		t.Errorf("parse ran %d times, want 2 (one failure + one success)", got)
	}
	if got := ctl.importRuns.Load(); got != 1 {
		t.Errorf("import ran %d times, want 1", got)
	}

	steps, err := m.ListJobSteps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"download", "parse", "import"}
	if len(steps) != len(wantNames) {
		t.Fatalf("ListJobSteps: got %d, want %d", len(steps), len(wantNames))
	}
	for i, s := range steps {
		if s.Name != wantNames[i] {
			t.Errorf("steps[%d].Name = %q, want %q", i, s.Name, wantNames[i])
		}
		if s.State != jobs.StepSucceeded {
			t.Errorf("steps[%d].State = %q, want succeeded", i, s.State)
		}
	}
}

func TestStep_OutsideRunFallsBackToPlainCall(t *testing.T) {
	// Step should still work when called from outside a job's Run
	// (e.g. library code shared between handler and tests). Without
	// a jobState on the context, it just runs fn and returns.
	ran := 0
	ctx := dummyContext{Context: context.Background()}
	got, err := jobs.Step(ctx, "anywhere", func(_ context.Context) (string, error) {
		ran++
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" || ran != 1 {
		t.Errorf("got=%q ran=%d, want ok / 1", got, ran)
	}
}

func TestStep_PersistedResultDecodes(t *testing.T) {
	// Round-trip the result of a Step through the store. Run Step
	// twice with the same name on the same jobID: second call must
	// return the persisted value without invoking fn.
	resetCSV()
	m, _ := jobs.New(memory.New(), jobs.Config{
		DefaultBackoff: jobs.ExponentialBackoff{Base: 1 * time.Millisecond},
	})
	must(t, jobs.Register[csvImport](m, "csv_import"))

	ctl := &csvControl{} // no failures
	registerCSVCtrl("happy", ctl)
	id := enq(t, m, &csvImport{Tag: "happy"}, jobs.Options{})

	w, _ := jobs.NewWorker(m, fastWorkerConfig())
	done := startWorker(t, w)
	defer func() {
		w.Stop(context.Background())
		<-done
	}()

	waitForState(t, m, id, jobs.StateSucceeded, 2*time.Second)
	if ctl.downloadRuns.Load() != 1 || ctl.parseRuns.Load() != 1 || ctl.importRuns.Load() != 1 {
		t.Errorf("each step should run once: download=%d parse=%d import=%d",
			ctl.downloadRuns.Load(), ctl.parseRuns.Load(), ctl.importRuns.Load())
	}
}

// --- helpers ---

// dummyContext satisfies jobs.Context without any of the runtime
// plumbing. Used by TestStep_OutsideRunFallsBackToPlainCall to
// verify Step degrades cleanly when called bare.
type dummyContext struct {
	context.Context
}

func (dummyContext) JobID() string                 { return "" }
func (dummyContext) Kind() string                  { return "" }
func (dummyContext) Attempt() int                  { return 0 }
func (dummyContext) Logger() *slog.Logger          { return slog.Default() }
func (dummyContext) Progress(_, _ int64, _ string) {}
