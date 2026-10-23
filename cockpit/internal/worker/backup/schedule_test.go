package backup

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduledSweepRespectsIntervalAndManualRun(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	file := filepath.Join(root, "report.md")
	writeFile(t, file, 10)
	f.watches = []map[string]any{watchRow("daily", root, map[string]any{"intervalMinutes": 1440, "lastSweepAt": time.Now().UTC().Format(time.RFC3339Nano)})}
	m := managerFor(t, f, root, allow)
	m.sweep(context.Background(), "wkr-1", false)
	if len(f.uploadedPaths()) != 0 {
		t.Fatal("scanned before schedule was due")
	}
	m.SweepOnce(context.Background(), "wkr-1")
	if len(f.uploadedPaths()) != 1 {
		t.Fatal("manual run did not bypass the schedule")
	}
}

func TestDueAfterRestartAndOfflinePeriod(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		last    time.Time
		minutes int
		due     bool
	}{
		{time.Time{}, 1440, true}, {now.Add(-24 * time.Hour), 1440, true}, {now.Add(-23 * time.Hour), 1440, false},
		{now.Add(-7 * 24 * time.Hour), 1440, true}, {now.Add(-5 * time.Minute), 0, true}, {now.Add(time.Hour), 1440, true},
	} {
		w := Watch{LastSweepAt: tc.last, IntervalMinutes: tc.minutes}
		if w.Due(now) != tc.due {
			t.Errorf("%+v due=%v", w, w.Due(now))
		}
	}
}
