package memql

// A routed message crosses replicas (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "Testing",
// cross-node). Routing on one replica and execution on another share no
// memory: RunAsk records the person's message on replica A, and the goal's
// agent loop runs on replica B, an engine booted on its own -- none of A's
// caches, coordination pool, scripted model or stubbed builtins -- which
// reads and acknowledges the message under the run's own RunContext. A's
// snapshot then shows what B did.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// hopReplica is a second engine on the routing fixture's database, booted on
// its own.
func hopReplica(t *testing.T, routing *MemQLEngine) *MemQLEngine {
	t.Helper()
	b, _, _ := readMergeTestEngine(t)
	require.NotSame(t, routing, b)
	return b
}

// hopStep is what an agent loop on any replica runs one step of a goal
// under: the run's own RunContext, as the goal's owner.
func hopStep(f *routeFixture, goalID, runID string) context.Context {
	return common.ContextWithRun(f.ctx, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: f.user, StepKey: "answer", Mode: common.RunModeLive})
}

// hopSnapshot is the person's conversation as e's askConversationSnapshot
// projects it, by turn id.
func hopSnapshot(t *testing.T, e *MemQLEngine, f *routeFixture) map[string]AskTurn {
	t.Helper()
	call, err := parser.RenderCall("askConversationSnapshot", map[string]any{"conversationId": f.conversation})
	require.NoError(t, err)
	result, err := e.Execute(f.ctx, "builtin "+call)
	require.NoError(t, err)
	rows := MaterializeRows(result.OutputPayload())
	require.Len(t, rows, 1)
	raw, err := json.Marshal(rows[0]["transcript"])
	require.NoError(t, err)
	var transcript askTranscript
	require.NoError(t, json.Unmarshal(raw, &transcript))
	turns := map[string]AskTurn{}
	for _, turn := range transcript.Turns {
		turns[turn.ID] = turn
	}
	return turns
}

// The owner's example across replicas: "Actually I'm in Phoenix" is routed
// and recorded on A while the Tucson goal's agent loop runs on B. B reads the
// message at its next step, word for word under the turn's id, and
// acknowledges it; the person's conversation, read on A, labels it Received.
// A receipt is not a cursor: the run keeps the message for the rest of its
// life.
func TestRoutedGuidanceCrossesReplicas(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."}))
	b := hopReplica(t, f.e)
	goal, run := f.goal("weather", "What's the weather in Tucson?", "Weather in Tucson", "running")
	step := hopStep(f, goal, run)

	// B's loop is already running the goal and has looked for guidance once.
	before, err := b.ReadWorkGuidance(step)
	require.NoError(t, err)
	require.Empty(t, before)

	_, answer, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Switching the weather check to Phoenix.", answer)
	require.Empty(t, f.intake, "no second goal")
	target := AskRouteTarget{GoalID: goal, RunID: run, Title: "Weather in Phoenix", Status: routeQueued, Text: "Actually I'm in Phoenix"}
	require.Equal(t, []AskRouteTarget{target}, hopSnapshot(t, f.e, f)["phoenix"].Routed.Targets)

	read, err := b.ReadWorkGuidance(step)
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "phoenix", Prompt: "Actually I'm in Phoenix"}}, read, "B reads what A recorded")
	require.NoError(t, b.AcknowledgeWorkGuidance(step, []string{read[0].ID}))

	routed := hopSnapshot(t, f.e, f)["phoenix"]
	target.Status = routeReceived
	require.Equal(t, []AskRouteTarget{target}, routed.Routed.Targets, "A's snapshot shows B's receipt")
	require.Equal(t, "done", routed.State)
	require.Equal(t, "Switching the weather check to Phoenix.", routed.Answer)

	again, err := b.ReadWorkGuidance(step)
	require.NoError(t, err)
	require.Equal(t, read, again, "the run keeps an acknowledged message")
}

// A goal still preparing takes a message as work guidance, and its first
// execution reads it on another replica. A records it while the run compiles,
// with no template and no fingerprint yet; B completes the compile -- running,
// the template compile chose, its fingerprint and compile's classification --
// and its first step reads the message. The label stays Queued through the
// compile and turns Received only once that step has read it, and the late
// compile keeps the title the message gave the goal.
func TestPreparingGuidanceCrossesReplicas(t *testing.T) {
	f := newRouteFixture(t, routeAnswer(map[string]any{"updates": []any{routeUpdate("g1", "Weather in Phoenix")}, "reply": "Switching the weather check to Phoenix."}))
	b := hopReplica(t, f.e)
	// Intake opened the goal; compile has not chosen its template.
	goal, run := id.NewShortId(), id.NewShortId()
	f.write("createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "work.compile", "templateFingerprint": "", "triggeredBy": "manual", "status": "compiling", "mode": "live",
		"input": map[string]any{"conversation": map[string]any{"id": f.conversation}}, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	f.record(AskTurn{ID: "weather", Prompt: "What's the weather in Tucson?", GoalID: goal, RunID: run, State: "queued", Background: true})

	_, answer, err := f.ask("phoenix", "Actually I'm in Phoenix", AskRoute{})
	require.NoError(t, err)
	require.Equal(t, "Switching the weather check to Phoenix.", answer)
	require.Empty(t, f.intake, "no second goal")
	require.Len(t, f.model.prompts, 1)
	require.Contains(t, f.model.prompts[0], "State: preparing", "routing is told the goal is preparing")
	target := AskRouteTarget{GoalID: goal, RunID: run, Title: "Weather in Phoenix", Status: routeQueued, Text: "Actually I'm in Phoenix"}
	require.Equal(t, []AskRouteTarget{target}, f.turn("phoenix").Routed.Targets, "recorded while the run compiles")

	// Compile completes on B, as the work integration records it.
	compiled, err := parser.RenderCall("updateWorkRun", map[string]any{"runId": run, "status": "running", "automationName": "weatherReport", "templateFingerprint": "weather-report-v1", "goalSignature": "weather-report",
		"classification": map[string]any{"workload": "research", "workTitle": "Weather in Tucson", "acknowledgement": "I'll check the weather in Tucson."}})
	require.NoError(t, err)
	_, err = b.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+compiled)
	require.NoError(t, err)
	require.Equal(t, []AskRouteTarget{target}, hopSnapshot(t, f.e, f)["phoenix"].Routed.Targets, "still queued after the compile, under the message's title")

	step := hopStep(f, goal, run)
	read, err := b.ReadWorkGuidance(step)
	require.NoError(t, err)
	require.Equal(t, []WorkGuidance{{ID: "phoenix", Prompt: "Actually I'm in Phoenix"}}, read, "the first execution reads what was recorded while it compiled")
	require.NoError(t, b.AcknowledgeWorkGuidance(step, []string{read[0].ID}))
	target.Status = routeReceived
	require.Equal(t, []AskRouteTarget{target}, hopSnapshot(t, f.e, f)["phoenix"].Routed.Targets, "A's snapshot shows B's receipt")
}
