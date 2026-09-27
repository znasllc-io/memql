package work

import (
	"reflect"
	"testing"
)

// TestGoalInputDropsOnlyWhatAReplayAdded: the goal's own input survives, the
// procedure id compile laid over it does not, the row read is left as it was,
// and a run whose only variable was the replay's has no input at all.
func TestGoalInputDropsOnlyWhatAReplayAdded(t *testing.T) {
	row := map[string]any{"day": "2026-09-04", ProcedureConstructVariable: "v1:authoring:construct:c1"}
	got := GoalInput(row)
	if want := map[string]any{"day": "2026-09-04"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GoalInput = %v, want %v", got, want)
	}
	if _, kept := row[ProcedureConstructVariable]; !kept {
		t.Fatalf("GoalInput changed the row it was handed: %v", row)
	}
	if got := GoalInput(map[string]any{ProcedureConstructVariable: "c1"}); got != nil {
		t.Fatalf("a run whose only variable was the replay's has no input, got %v", got)
	}
	if got := GoalInput(nil); got != nil {
		t.Fatalf("no variables is no input, got %v", got)
	}
}
