package memql

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestStructuredFleetCallJournalsLocalUsage(t *testing.T) {
	fleet := &stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: `{"answer":"draft"}`}
	e := engineWithFleetDefaultPrompt(t, fleet, "")
	journal := newCountingJournal()
	e.SetModelCallJournal(journal)
	ctx := common.ContextWithRun(userCtx("alice"), common.RunContext{
		RunId: "materialization-run", GoalId: "materialization-goal", StepKey: "compose", OwnerUserId: "alice", Mode: common.RunModeLive,
	})
	_, err := e.InvokeAIStructured(ctx, "localConductorTurn", map[string]any{"utterance": "write a document"}, "draft", json.RawMessage(`{"type":"object"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.lastReq.Messages) < 2 || fleet.lastReq.Messages[len(fleet.lastReq.Messages)-1].Role != "user" {
		t.Fatalf("structured recovery cannot run on runtimes requiring a user query: %+v", fleet.lastReq.Messages)
	}
	rows := journal.rows["materialization-run"]
	if len(rows) != 1 {
		t.Fatalf("model journal has %d rows", len(rows))
	}
	got := rows[0]
	if got.Served != "local" || got.InputTokens != 3 || got.OutputTokens != 5 || got.Model != "llama3.1:8b" {
		t.Fatalf("local model and actual usage lost in journal: %+v", got)
	}
}

func TestStructuredResultPreservesAttributionOnReplay(t *testing.T) {
	fleet := &stubFleet{models: []FleetModel{onlineModel("llama3.1:8b", true)}, answer: `{"answer":"original draft"}`}
	e := engineWithFleetDefaultPrompt(t, fleet, "")
	journal := newCountingJournal()
	e.SetModelCallJournal(journal)
	run := common.RunContext{RunId: "original", GoalId: "goal", StepKey: "compose", OwnerUserId: "alice", Mode: common.RunModeLive}
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, ExplicitProvider: "fleet:llama3.1:8b"}
	messages := []common.ChatMessage{{Role: "user", Content: "Write the document"}}
	schema := common.StructuredSchema{Name: "draft", Schema: json.RawMessage(`{"type":"object"}`), Strict: true}
	live, err := e.CallAIStructured(common.ContextWithRun(userCtx("alice"), run), req, messages, schema)
	if err != nil {
		t.Fatal(err)
	}
	if live.Resolution.ProviderName != "fleet:llama3.1:8b" || live.Usage.InputTokens != 3 || live.Usage.OutputTokens != 5 || !live.Usage.Reported || live.Served != "local" {
		t.Fatalf("live metadata missing: %+v", live)
	}
	// A changed provider answer makes any accidental live call observable.
	fleet.answer = `{"answer":"must not be called during replay"}`
	run.RunId, run.SourceRunId, run.SourceGoalId = "replay", "original", "goal"
	run.Mode = common.RunModeReplay
	replayed, err := e.CallAIStructured(common.ContextWithRun(userCtx("alice"), run), req, messages, schema)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Text != live.Text || replayed.Served != "journal" || replayed.Resolution.Model != live.Resolution.Model || replayed.Usage != live.Usage {
		t.Fatalf("replayed content or provenance changed: live=%+v replay=%+v", live, replayed)
	}
	if len(journal.rows["replay"]) != 1 || journal.rows["replay"][0].Served != "journal" {
		t.Fatal("replay did not receive its own journal entry")
	}
}
