package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplanFinishedResultAppendsWorkWithoutReplayingCompletedEffects(t *testing.T) {
	rc := replanFixture()
	rc.Recorded["draft"] = "done"
	rc.CompletedSteps = append(rc.CompletedSteps, map[string]any{"key": "draft", "call": map[string]any{"construct": "function", "name": "runAgentTurn"}, "result": map[string]any{"value": "saved draft"}})
	rc.FailedStep = map[string]any{"key": "draft", "symptom": "plan", "finishedResultFeedback": true, "errorMessage": "The owner asked to deliver the saved draft."}
	source := strings.Replace(replanSource, "  write :=", "  draft := builtin runAgentTurn(agentId: \"agent-1\", prompt: \"draft the summary\")\n  write :=", 1)
	eng := &replanEngine{answer: replanAnswer(t, source, false)}
	rec := &remedyRecorder{context: rc}
	require.True(t, newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "Deliver the saved draft"))
	require.Len(t, rec.installed, 1)
	require.Equal(t, "write", rec.installed[0].ResumeAt)
	require.Equal(t, []string{"gather", "draft", "write"}, rec.installed[0].StepKeys)
	require.Empty(t, rec.asked)
	// Removing a completed effect from the saved prefix must still fail Gate 1.
	bad := strings.Replace(source, "  draft := builtin runAgentTurn(agentId: \"agent-1\", prompt: \"draft the summary\")\n", "", 1)
	eng = &replanEngine{answer: replanAnswer(t, bad, false)}
	rec = &remedyRecorder{context: rc}
	require.True(t, newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "Deliver the saved draft"))
	require.Empty(t, rec.installed)
	require.NotEmpty(t, rec.asked)
}
