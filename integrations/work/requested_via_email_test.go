package work

import (
	"slices"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// TestRequestedViaAcceptsEmail: an email composition's goal arrives through
// `email` (campaignComposeEmail opens it, with the composeEmailRun template).
// The value is declared on v1:work:goal, accepted at the door, and written onto
// the goal both when the work spine opens it directly and when a caller names
// it.
func TestRequestedViaAcceptsEmail(t *testing.T) {
	if err := validRequestedVia("email"); err != nil {
		t.Fatalf("validRequestedVia(email): %v", err)
	}
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	if enum := enumOf(t, goalConcept, "requestedVia"); !slices.Contains(enum, "email") {
		t.Fatalf("v1:work:goal.requestedVia declares %v, without email", enum)
	}

	i, eng := newTestIntegration(t)
	if _, _, err := i.OpenDirectGoal(callerContext("u-alice"), DirectGoal{
		OwnerUserId: "u-alice", Statement: "Write the spring email", AutomationName: "composeEmailRun",
		RequestedVia: "email", AccountIds: []string{"acme"}, Input: map[string]any{"templateId": "t-1"},
	}); err != nil {
		t.Fatalf("OpenDirectGoal: %v", err)
	}
	if got := eng.callTo(t, "createWorkGoal").Args(t)["requestedVia"]; got != "email" {
		t.Errorf("the direct goal was written with requestedVia %v", got)
	}

	i, eng = newTestIntegration(t)
	if _, err := i.handleCreateGoal(callerContext("u-alice"), map[string]any{"statement": "Write the spring email", "requestedVia": "email"}, 0); err != nil {
		t.Fatalf("createGoal with requestedVia email: %v", err)
	}
	if got := eng.callTo(t, "createWorkGoal").Args(t)["requestedVia"]; got != "email" {
		t.Errorf("the goal was written with requestedVia %v", got)
	}
}
