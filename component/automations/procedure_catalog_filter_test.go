package automations

import "testing"

// catalogSucceededSections (epic memql#5414, #5418) catalogues the sections a
// succeeded goal run worked out live. Its @filter decides when it fires, and
// the automation corpus never evaluates a filter, so the decisions are held
// here, through the path the scheduler fires through (see
// procedure_ladder_filters_test.go for the helpers and the event's shape):
//
//   - on the TRANSITION to succeeded, once: a run keeps being written after it
//     succeeds, and each write is a node.updated event;
//   - only for a run with an owner (whose authority the handler borrows), a
//     goal, and a compiled draft of its own -- the only kind of run whose
//     template can hold a section worked out live.
func TestCatalogSucceededSectionsFiresOnTheTransitionOfADraftedGoalRunOnly(t *testing.T) {
	a := loadedProcedureAutomation(t, "catalogSucceededSections")
	const run = "v1:work:run"
	const owner, goal, draft = "v1:identity:user:alice", "v1:work:goal:g1", "v1:authoring:construct:c1"
	for _, tc := range []struct {
		name      string
		status    string
		oldStatus string
		owner     string
		goal      string
		draft     string
		fires     bool
	}{
		{"running to succeeded", "succeeded", "running", owner, goal, draft, true},
		{"a first write that is already succeeded", "succeeded", "", owner, goal, draft, true},
		{"succeeded to succeeded", "succeeded", "succeeded", owner, goal, draft, false},
		{"running to failed", "failed", "running", owner, goal, draft, false},
		{"a heartbeat while running", "running", "running", owner, goal, draft, false},
		{"a succeeded run with no owner", "succeeded", "running", "", goal, draft, false},
		{"a succeeded automation run with no goal", "succeeded", "running", owner, "", draft, false},
		// A goal the catalog served whole, or a learned procedure's replay:
		// no draft of its own, so no section was worked out in it.
		{"a succeeded goal run with no draft", "succeeded", "running", owner, goal, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"status": tc.status, "automationName": "x"}
			if tc.owner != "" {
				fields["ownerUserId"] = tc.owner
			}
			if tc.goal != "" {
				fields["goalId"] = tc.goal
			}
			if tc.draft != "" {
				fields["templateConstructId"] = tc.draft
			}
			ev := graphUpdatedEvent(run, run+":r1", fields, tc.oldStatus)
			if got := filterFires(t, a, ev); got != tc.fires {
				t.Fatalf("catalogSucceededSections fired=%v for %s, want %v", got, tc.name, tc.fires)
			}
		})
	}
}
