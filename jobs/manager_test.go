package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/moostackhq/go/jobs"
	"github.com/moostackhq/go/jobs/store/memory"
)

// --- test fixtures ---

type sendEmail struct {
	UserID int64 `json:"user_id"`
}

func (j *sendEmail) Run(_ jobs.Context) error { return nil }

type generateReport struct {
	Period string `json:"period"`
}

func (j *generateReport) Run(_ jobs.Context) error { return nil }

func newManager(t *testing.T) *jobs.Manager {
	t.Helper()
	m, err := jobs.New(memory.New(), jobs.Config{})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	return m
}

// --- New ---

func TestNew_NilStore(t *testing.T) {
	if _, err := jobs.New(nil, jobs.Config{}); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestNew_SchedulerIntervalClampedToMax(t *testing.T) {
	// User asks for 1h tick cadence; manager must clamp to
	// MaxSchedulerInterval (cron's resolution is one minute,
	// anything beyond that routinely misses schedules) and log a
	// warning. Verify via the warning log; testing the actual tick
	// cadence would mean waiting MaxSchedulerInterval (slow).
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, err := jobs.New(memory.New(), jobs.Config{
		Logger:            logger,
		SchedulerInterval: 1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "SchedulerInterval too large") {
		t.Errorf("expected clamp warning in log; got: %q", buf.String())
	}
}

func TestNew_RejectsNegativeDefaultMaxAttempts(t *testing.T) {
	// A negative DefaultMaxAttempts would otherwise slip through New
	// silently and surface as a confusing Enqueue-side error
	// attributing the bug to the caller. Fail fast in New instead.
	_, err := jobs.New(memory.New(), jobs.Config{DefaultMaxAttempts: -1})
	if err == nil {
		t.Fatal("expected error for negative DefaultMaxAttempts")
	}
	if !strings.Contains(err.Error(), "DefaultMaxAttempts") {
		t.Errorf("error should mention DefaultMaxAttempts; got: %v", err)
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	// Defaults are observable via Enqueue: a job enqueued with the
	// zero Options should land on the default queue.
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &sendEmail{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Queue != "default" {
		t.Errorf("Queue = %q, want %q", info.Queue, "default")
	}
	if info.MaxAttempts != 25 {
		t.Errorf("MaxAttempts = %d, want 25", info.MaxAttempts)
	}
}

// --- Register ---

func TestRegister_Success(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestRegister_DuplicateName(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "k"); err != nil {
		t.Fatal(err)
	}
	err := jobs.Register[generateReport](m, "k")
	if !errors.Is(err, jobs.ErrKindAlreadyRegistered) {
		t.Errorf("want ErrKindAlreadyRegistered, got %v", err)
	}
}

func TestRegister_DuplicateType(t *testing.T) {
	// Registering the same Go type under a second name is a
	// programmer error: Enqueue would not know which kind to pick.
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Register[sendEmail](m, "send_email_v2"); err == nil {
		t.Error("expected error for duplicate type registration")
	}
}

func TestRegister_NilManagerOrEmptyName(t *testing.T) {
	if err := jobs.Register[sendEmail](nil, "x"); err == nil {
		t.Error("expected error for nil manager")
	}
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, ""); err == nil {
		t.Error("expected error for empty name")
	}
}

// --- Enqueue ---

func TestEnqueue_BasicRoundTrip(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &sendEmail{UserID: 42})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("Enqueue returned empty id")
	}
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if info.Kind != "send_email" {
		t.Errorf("Kind = %q, want send_email", info.Kind)
	}
	if info.State != jobs.StateAvailable {
		t.Errorf("State = %q, want available", info.State)
	}
}

func TestEnqueue_UnregisteredKind(t *testing.T) {
	m := newManager(t)
	_, err := m.Enqueue(context.Background(), &sendEmail{UserID: 1})
	if !errors.Is(err, jobs.ErrUnregistered) {
		t.Errorf("want ErrUnregistered, got %v", err)
	}
}

// valueRunJob has a VALUE-receiver Run. This is legal (both
// valueRunJob and *valueRunJob then satisfy jobs.Job), but it's
// also the only way for Enqueue(valueRunJob{}) to compile and
// reach the value-vs-pointer hint code path. Register stores the
// constructor for *valueRunJob.
type valueRunJob struct {
	X int `json:"x"`
}

func (j valueRunJob) Run(_ jobs.Context) error { return nil }

func TestEnqueue_ValueInsteadOfPointer_HintsCaller(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[valueRunJob](m, "value_run"))

	// Passing the value type satisfies jobs.Job at compile time but
	// misses the type→name lookup (registered under *valueRunJob).
	// Should get a hinting error, not the bare "unregistered" line.
	_, err := m.Enqueue(context.Background(), valueRunJob{X: 1})
	if err == nil {
		t.Fatal("expected error for value type Enqueue")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pass a pointer") {
		t.Errorf("error %q does not hint at the pointer mistake", msg)
	}
	if errors.Is(err, jobs.ErrUnregistered) {
		t.Error("hint error should not match ErrUnregistered (would mislead callers)")
	}

	// Sanity: the pointer form still works.
	if _, err := m.Enqueue(context.Background(), &valueRunJob{X: 2}); err != nil {
		t.Errorf("pointer Enqueue failed: %v", err)
	}
}

func TestEnqueue_NegativeMaxAttemptsRejected(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[sendEmail](m, "send_email"))

	_, err := m.Enqueue(context.Background(), &sendEmail{UserID: 1}, jobs.Options{
		MaxAttempts: -1,
	})
	if err == nil {
		t.Fatal("expected error for negative MaxAttempts")
	}
	if !strings.Contains(err.Error(), "MaxAttempts must be >= 0") {
		t.Errorf("error %q should hint at the validation rule", err)
	}

	// Zero is still valid (defaults to manager's DefaultMaxAttempts).
	if _, err := m.Enqueue(context.Background(), &sendEmail{UserID: 2}, jobs.Options{
		MaxAttempts: 0,
	}); err != nil {
		t.Errorf("MaxAttempts=0 should be accepted (uses default), got %v", err)
	}
}

func TestEnqueue_NilJob(t *testing.T) {
	m := newManager(t)
	if _, err := m.Enqueue(context.Background(), nil); err == nil {
		t.Error("expected error for nil job")
	}
}

func TestEnqueue_ScheduledWhenDelayed(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &sendEmail{}, jobs.Options{
		Delay: 1 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := m.GetJob(context.Background(), id)
	if info.State != jobs.StateScheduled {
		t.Errorf("State = %q, want scheduled", info.State)
	}
	if d := time.Until(info.AvailableAt); d < 50*time.Minute {
		t.Errorf("AvailableAt %v is too soon; want ~1h out", info.AvailableAt)
	}
}

func TestEnqueue_RunAtOverridesDelay(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	id, err := m.Enqueue(context.Background(), &sendEmail{}, jobs.Options{
		Delay: 1 * time.Hour,
		RunAt: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := m.GetJob(context.Background(), id)
	if !info.AvailableAt.Equal(target) {
		t.Errorf("AvailableAt = %v, want %v", info.AvailableAt, target)
	}
}

func TestEnqueue_OptionsRespected(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Enqueue(context.Background(), &sendEmail{}, jobs.Options{
		Queue:       "emails",
		Priority:    5,
		MaxAttempts: 3,
		Timeout:     30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := m.GetJob(context.Background(), id)
	if info.Queue != "emails" {
		t.Errorf("Queue = %q", info.Queue)
	}
	if info.Priority != 5 {
		t.Errorf("Priority = %d", info.Priority)
	}
	if info.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d", info.MaxAttempts)
	}
	if info.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v", info.Timeout)
	}
}

func TestEnqueue_UniqueKeyCollision(t *testing.T) {
	m := newManager(t)
	if err := jobs.Register[sendEmail](m, "send_email"); err != nil {
		t.Fatal(err)
	}
	first, err := m.Enqueue(context.Background(), &sendEmail{UserID: 1}, jobs.Options{
		UniqueKey: "import:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Enqueue(context.Background(), &sendEmail{UserID: 2}, jobs.Options{
		UniqueKey: "import:42",
	})
	if !errors.Is(err, jobs.ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if second != first {
		t.Errorf("collision returned id %q, want existing %q", second, first)
	}
	var dup *jobs.DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("error did not unwrap to *DuplicateError")
	}
	if dup.ExistingID != first || dup.UniqueKey != "import:42" || dup.Kind != "send_email" {
		t.Errorf("DuplicateError = %+v", dup)
	}
}

// EnqueueTx on the memory backend returning ErrUnsupported is
// tested at the store level in store/memory/store_test.go; building
// a real *sql.Tx here would require importing a SQL driver, which
// comes in Phase 10.

// --- GetJob / ListJobs ---

func TestGetJob_NotFound(t *testing.T) {
	m := newManager(t)
	_, err := m.GetJob(context.Background(), "nonexistent")
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestListJobs_FilterByQueueAndKind(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[sendEmail](m, "send_email"))
	must(t, jobs.Register[generateReport](m, "gen_report"))

	enq(t, m, &sendEmail{UserID: 1}, jobs.Options{Queue: "emails"})
	enq(t, m, &sendEmail{UserID: 2}, jobs.Options{Queue: "emails"})
	enq(t, m, &generateReport{Period: "daily"}, jobs.Options{Queue: "reports"})
	enq(t, m, &generateReport{Period: "weekly"}, jobs.Options{Queue: "reports"})

	page, err := m.ListJobs(context.Background(), jobs.JobFilter{
		Queues: []string{"emails"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Jobs) != 2 {
		t.Errorf("Queues filter: got %d, want 2", len(page.Jobs))
	}

	page, _ = m.ListJobs(context.Background(), jobs.JobFilter{
		Kinds: []string{"gen_report"},
	})
	if len(page.Jobs) != 2 {
		t.Errorf("Kinds filter: got %d, want 2", len(page.Jobs))
	}

	page, _ = m.ListJobs(context.Background(), jobs.JobFilter{
		Queues: []string{"emails"},
		Kinds:  []string{"send_email"},
	})
	if len(page.Jobs) != 2 {
		t.Errorf("combined filter: got %d, want 2", len(page.Jobs))
	}
}

func TestListJobs_FilterByState(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[sendEmail](m, "send_email"))

	// One available, one scheduled.
	enq(t, m, &sendEmail{UserID: 1}, jobs.Options{})
	enq(t, m, &sendEmail{UserID: 2}, jobs.Options{Delay: 1 * time.Hour})

	page, _ := m.ListJobs(context.Background(), jobs.JobFilter{
		States: []jobs.State{jobs.StateAvailable},
	})
	if len(page.Jobs) != 1 {
		t.Errorf("StateAvailable: got %d, want 1", len(page.Jobs))
	}
	page, _ = m.ListJobs(context.Background(), jobs.JobFilter{
		States: []jobs.State{jobs.StateScheduled},
	})
	if len(page.Jobs) != 1 {
		t.Errorf("StateScheduled: got %d, want 1", len(page.Jobs))
	}
}

func TestListJobs_PaginationRoundTrip(t *testing.T) {
	const total = 1000
	m := newManager(t)
	must(t, jobs.Register[sendEmail](m, "send_email"))

	// Spread CreatedAt enough that ordering is deterministic. Memory
	// store uses time.Now() inside Insert via Manager; back-to-back
	// enqueues might share a nanosecond on some platforms, but the
	// secondary id sort ensures we still get a stable order.
	for i := 0; i < total; i++ {
		enq(t, m, &sendEmail{UserID: int64(i)}, jobs.Options{})
	}

	// Walk the full set with a small page size and assemble it.
	seen := make(map[string]struct{}, total)
	cursor := ""
	for {
		page, err := m.ListJobs(context.Background(), jobs.JobFilter{
			Limit:  37, // an awkward page size to exercise tail
			Cursor: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range page.Jobs {
			if _, dup := seen[j.ID]; dup {
				t.Fatalf("id %s appeared twice", j.ID)
			}
			seen[j.ID] = struct{}{}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Errorf("pagination saw %d jobs, want %d", len(seen), total)
	}
}

func TestListJobs_LimitNormalised(t *testing.T) {
	m := newManager(t)
	must(t, jobs.Register[sendEmail](m, "send_email"))
	for i := 0; i < 5; i++ {
		enq(t, m, &sendEmail{UserID: int64(i)}, jobs.Options{})
	}
	// Limit=0 should default to DefaultJobsLimit (100), so all 5 fit.
	page, _ := m.ListJobs(context.Background(), jobs.JobFilter{Limit: 0})
	if len(page.Jobs) != 5 || page.NextCursor != "" {
		t.Errorf("default limit: got %d jobs, cursor=%q", len(page.Jobs), page.NextCursor)
	}
}

// --- cursor helpers ---

func TestCursor_EncodeDecode_RoundTrip(t *testing.T) {
	now := time.Now().UTC()
	c := jobs.EncodeJobsCursor(now, "abc-123")
	gotTime, gotID, err := jobs.DecodeJobsCursor(c)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(now) {
		t.Errorf("time round trip: got %v, want %v", gotTime, now)
	}
	if gotID != "abc-123" {
		t.Errorf("id round trip: got %q, want %q", gotID, "abc-123")
	}
}

func TestCursor_DecodeEmpty(t *testing.T) {
	gotTime, gotID, err := jobs.DecodeJobsCursor("")
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.IsZero() || gotID != "" {
		t.Errorf("empty cursor should yield zero values, got (%v, %q)", gotTime, gotID)
	}
}

func TestCursor_DecodeMalformed(t *testing.T) {
	if _, _, err := jobs.DecodeJobsCursor("not base64!"); err == nil {
		t.Error("expected error for non-base64 cursor")
	}
	// "abc" base64-encoded: valid base64, but no ':' separator.
	if _, _, err := jobs.DecodeJobsCursor("YWJj"); err == nil {
		t.Error("expected error for malformed cursor body")
	}
}

// --- helpers ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func enq(t *testing.T, m *jobs.Manager, job jobs.Job, opts jobs.Options) string {
	t.Helper()
	id, err := m.Enqueue(context.Background(), job, opts)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return id
}

