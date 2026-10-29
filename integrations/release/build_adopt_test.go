package release

import (
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// TestAdoptableBuildRun pins when a saved release replaces the run it recorded
// with a newer attempt of the same release
// (docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md).
func TestAdoptableBuildRun(t *testing.T) {
	failed := pl.ReleaseBuildAttempt{ID: "a1", Attempt: 1, Status: "completed", Conclusion: "failure"}
	descendant := func(earlier ...pl.ReleaseBuildAttempt) pl.ReleaseBuildRun {
		return pl.ReleaseBuildRun{ID: "a2", Attempt: 2, Status: "in_progress", RerunOf: "a1", Chain: earlier}
	}
	for _, tc := range []struct {
		name     string
		record   func(*releaseBuildRecord)
		observed pl.ReleaseBuildRun
		want     bool
	}{
		{"descendant of a failed attempt", nil, descendant(failed), true},
		{"descendant of a cancelled attempt", nil,
			descendant(pl.ReleaseBuildAttempt{ID: "a1", Attempt: 1, Status: "completed", Conclusion: "cancelled", CancelRequested: true}), true},
		{"recorded attempt succeeded", nil,
			descendant(pl.ReleaseBuildAttempt{ID: "a1", Attempt: 1, Status: "completed", Conclusion: "success"}), false},
		{"recorded attempt still running", nil,
			descendant(pl.ReleaseBuildAttempt{ID: "a1", Attempt: 1, Status: "in_progress"}), false},
		{"recorded run outside the chain", nil,
			descendant(pl.ReleaseBuildAttempt{ID: "a0", Attempt: 1, Status: "completed", Conclusion: "failure"}), false},
		{"empty chain", nil, descendant(), false},
		{"candidate prepared", func(r *releaseBuildRecord) { r.CandidateID = "sha256:" + strings.Repeat("a", 64) }, descendant(failed), false},
		{"publication started", func(r *releaseBuildRecord) { r.PublicationStarted = true }, descendant(failed), false},
		{"same run", nil, pl.ReleaseBuildRun{ID: "a1", Attempt: 1, Status: "completed", Conclusion: "failure"}, false},
		{"nothing recorded yet", func(r *releaseBuildRecord) { r.Run = nil }, descendant(failed), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorded := pl.ReleaseBuildRun{ID: "a1", Attempt: 1, Status: "in_progress"}
			record := releaseBuildRecord{Run: &recorded}
			if tc.record != nil {
				tc.record(&record)
			}
			if got := adoptableBuildRun(record, tc.observed); got != tc.want {
				t.Fatalf("adoptableBuildRun = %v, want %v", got, tc.want)
			}
		})
	}
}
