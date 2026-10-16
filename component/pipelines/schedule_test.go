package pipelines

import (
	"strings"
	"testing"
	"time"
)

func TestWeeklySlotPinsOneUTCWeekAcrossNewHeads(t *testing.T) {
	now := time.Date(2026, 12, 31, 23, 30, 0, 0, time.FixedZone("west", -7*3600))
	slot, err := ScheduleSlot("weekly", now)
	if err != nil || slot != "week:2026-12-28" {
		t.Fatalf("slot = %q, %v", slot, err)
	}
	first, err := ScheduledRunKey("Acme/Repo", strings.Repeat("a", 40), ModeFull, slot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ScheduledRunKey("acme/repo", strings.Repeat("b", 40), ModeFull, slot)
	if err != nil || first != second {
		t.Fatalf("new head repeated weekly scan: %s %s %v", first, second, err)
	}
	saved, ok := ScheduledSlotFromRunKey(first)
	if !ok || saved != slot {
		t.Fatalf("retry lost weekly slot: %q %v", saved, ok)
	}
	next, _ := ScheduleSlot("weekly", now.AddDate(0, 0, 7))
	nextKey, err := ScheduledRunKey("acme/repo", strings.Repeat("a", 40), ModeFull, next)
	if err != nil || nextKey == first {
		t.Fatalf("next week failed to rescan: %q %v", nextKey, err)
	}
	for _, invalid := range []string{"week:2026-12-29", "week:2026-2-2", "week:2026-12-28:extra", "2026-02-30"} {
		if _, err := ScheduleCadence(invalid); err == nil {
			t.Errorf("accepted slot %q", invalid)
		}
	}
}

func TestStageCadenceRequiresScheduledAdmission(t *testing.T) {
	for _, tc := range []struct {
		root, stage string
		on          []string
		valid       bool
	}{
		{"daily", "weekly", []string{"schedule"}, true},
		{"weekly", "", []string{"schedule"}, true},
		{"", "weekly", []string{"schedule"}, false},
		{"daily", "weekly", []string{"push"}, false},
		{"daily", "hourly", []string{"schedule"}, false},
	} {
		s := &Spec{Schedule: tc.root, Stages: []StageSpec{{Name: "scan", Schedule: tc.stage, On: tc.on, Steps: []StepSpec{{Name: "audit", Run: "audit"}}}}}
		if refusal := Validate(s); (refusal == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, refusal)
		}
	}
}
