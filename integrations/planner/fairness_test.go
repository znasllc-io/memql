package planner

import (
	"testing"
	"time"
)

func TestSelectFairnessVictim(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	minRun := 30 * time.Minute
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	cases := []struct {
		name    string
		running []runningSlot
		waiting int
		want    string // "" = no victim
	}{
		{
			name:    "no waiters -> never pass",
			running: []runningSlot{{PlanId: "p1", StartedAt: ago(2 * time.Hour)}},
			waiting: 0,
			want:    "",
		},
		{
			name:    "waiters but nothing running",
			running: nil,
			waiting: 3,
			want:    "",
		},
		{
			name:    "waiters but holder still within hysteresis floor",
			running: []runningSlot{{PlanId: "p1", StartedAt: ago(5 * time.Minute)}},
			waiting: 1,
			want:    "",
		},
		{
			name:    "single long holder is passed",
			running: []runningSlot{{PlanId: "p1", StartedAt: ago(2 * time.Hour)}},
			waiting: 1,
			want:    "p1",
		},
		{
			name: "longest-held holder yields first",
			running: []runningSlot{
				{PlanId: "young", StartedAt: ago(40 * time.Minute)},
				{PlanId: "oldest", StartedAt: ago(5 * time.Hour)},
				{PlanId: "mid", StartedAt: ago(90 * time.Minute)},
			},
			waiting: 2,
			want:    "oldest",
		},
		{
			name: "rows with zero/empty fields are skipped",
			running: []runningSlot{
				{PlanId: "", StartedAt: ago(5 * time.Hour)},
				{PlanId: "noStart"},
				{PlanId: "valid", StartedAt: ago(45 * time.Minute)},
			},
			waiting: 1,
			want:    "valid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectFairnessVictim(tc.running, tc.waiting, now, minRun)
			if tc.want == "" {
				if ok {
					t.Fatalf("expected no victim, got %q", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("victim = %q (ok=%v), want %q", got, ok, tc.want)
			}
		})
	}
}

func TestLoadFairnessConfig_Defaults(t *testing.T) {
	// No env set in the test process -> conservative defaults.
	cfg := loadFairnessConfig()
	if cfg.enabled {
		t.Fatal("fairness must default to DISABLED (opt-in)")
	}
	if cfg.minRun != time.Duration(defaultFairnessMinRunSeconds)*time.Second {
		t.Fatalf("minRun = %s, want %ds", cfg.minRun, defaultFairnessMinRunSeconds)
	}
	if cfg.sweep != time.Duration(defaultFairnessSweepSeconds)*time.Second {
		t.Fatalf("sweep = %s, want %ds", cfg.sweep, defaultFairnessSweepSeconds)
	}
}
