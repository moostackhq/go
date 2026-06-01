package jobs

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func TestPlanFires_CatchUpAllCapped(t *testing.T) {
	// A year of missed 1-minute ticks would be ~525,600 fires;
	// the cap turns that into catchUpAllMax + a capped=true flag.
	parsed, err := cron.ParseStandard("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	nextRun := now.Add(-365 * 24 * time.Hour)

	count, next, capped := planFires(parsed, nextRun, now, CatchUpAll)
	if !capped {
		t.Fatal("capped = false, want true")
	}
	if count != catchUpAllMax {
		t.Errorf("count = %d, want %d (the cap)", count, catchUpAllMax)
	}
	if !next.After(now) {
		t.Errorf("next = %v, want > now (scheduler must skip past the backlog)", next)
	}
}

func TestPlanFires_CatchUpAllUnderCap(t *testing.T) {
	// 30 missed 1-minute ticks should fire 30 times with no cap.
	parsed, err := cron.ParseStandard("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	nextRun := now.Add(-30 * time.Minute)

	count, next, capped := planFires(parsed, nextRun, now, CatchUpAll)
	if capped {
		t.Errorf("capped = true, want false for 30 missed ticks")
	}
	if count < 29 || count > 31 {
		// One-off slack: the boundary between "30 ticks ago exactly"
		// and the current minute boundary can flex by one.
		t.Errorf("count = %d, want ~30", count)
	}
	if !next.After(now) {
		t.Errorf("next = %v, want > now", next)
	}
}

func TestPlanFires_CatchUpSkipNeverFires(t *testing.T) {
	parsed, _ := cron.ParseStandard("* * * * *")
	now := time.Now()
	count, next, capped := planFires(parsed, now.Add(-10*time.Minute), now, CatchUpSkip)
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	if capped {
		t.Error("capped = true, want false")
	}
	if !next.After(now) {
		t.Errorf("next = %v, want > now", next)
	}
}

func TestPlanFires_CatchUpOnceFiresOnce(t *testing.T) {
	parsed, _ := cron.ParseStandard("* * * * *")
	now := time.Now()
	count, next, capped := planFires(parsed, now.Add(-30*time.Minute), now, CatchUpOnce)
	if count != 1 {
		t.Errorf("count = %d, want 1 (one fire regardless of missed ticks)", count)
	}
	if capped {
		t.Error("capped = true, want false")
	}
	if !next.After(now) {
		t.Errorf("next = %v, want > now", next)
	}
}
