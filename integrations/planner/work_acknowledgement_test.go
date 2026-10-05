package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type acknowledgementTestEngine struct {
	*countingCompileEngine
	candidate map[string]any
}

func (e *acknowledgementTestEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.Contains(q, "workAcknowledgementCache") {
		return []map[string]any{e.candidate}, nil
	}
	return e.countingCompileEngine.Execute(ctx, q)
}
func TestAcknowledgementCandidateDoesNotDecideClassificationOrAddModelCalls(t *testing.T) {
	engine := &acknowledgementTestEngine{countingCompileEngine: &countingCompileEngine{triage: map[string]any{
		"complexity": "trivial", "intent": "reply", "requiresFile": false, "workload": "lookup", "workTitle": "Report preference", "acknowledgement": "I’ll check the formatting preference you shared.",
	}}, candidate: map[string]any{"request": "Create a report", "workload": "research", "acknowledgement": "I’ll prepare your report."}}
	req := compileReq()
	req.Input = map[string]any{"conversation": map[string]any{"messages": []any{}}}
	out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	require.NoError(t, err)
	require.Equal(t, "lookup", out.Workload)
	require.Equal(t, "I’ll check the formatting preference you shared.", out.Acknowledgement)
	require.Len(t, engine.aiCalls, 1)
	require.Contains(t, engine.aiData[0]["acknowledgementCandidate"], "Create a report")
	// Missing/oversized prose does not manufacture a reply or fail useful work.
	engine.triage.(map[string]any)["acknowledgement"] = strings.Repeat("x", 281)
	out, err = (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	require.NoError(t, err)
	require.Empty(t, out.Acknowledgement)
}
