package memql

import "testing"

func TestVoiceActivityUsesTheSnapshotIdentityWithoutChangingTheJournal(t *testing.T) {
	stored := WorkEvent{Kind: "run", Name: "v1:work:run:run-a", Arguments: map[string]any{
		"runId": "v1:work:run:run-a", "goalId": "v1:work:goal:goal-a", "concept": "v1:work:run",
	}}
	wire := askVoiceActivityEvent("client-turn", stored)
	if wire.TurnID != "client-turn" || wire.Activity.Name != "run-a" || wire.Activity.Arguments["runId"] != "run-a" || wire.Activity.Arguments["goalId"] != "goal-a" || wire.Activity.Arguments["concept"] != "v1:work:run" {
		t.Fatalf("voice and snapshot identities differ: %+v", wire.Activity)
	}
	if stored.Arguments["runId"] != "v1:work:run:run-a" || stored.Name != "v1:work:run:run-a" {
		t.Fatal("wire conversion changed the stored activity")
	}
}
