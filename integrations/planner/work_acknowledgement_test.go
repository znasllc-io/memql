package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type acknowledgementTestEngine struct {
	*countingCompileEngine
	candidate map[string]any
	repair    any
	repairErr error
}

func (e *acknowledgementTestEngine) InvokeAI(ctx context.Context, name string, data map[string]any) (any, error) {
	response, err := e.countingCompileEngine.InvokeAI(ctx, name, data)
	if name == "workAcknowledgement" {
		return e.repair, e.repairErr
	}
	return response, err
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
}

func TestBackgroundAcknowledgementRepairIsBoundedAndDoesNotReclassify(t *testing.T) {
	for _, invalid := range []string{"", " \n", strings.Repeat("é", 281)} {
		t.Run(fmtName(invalid), func(t *testing.T) {
			engine := &acknowledgementTestEngine{countingCompileEngine: &countingCompileEngine{triage: map[string]any{
				"complexity": "trivial", "intent": "reply", "requiresFile": false, "workload": "lookup", "workTitle": "Check available projects", "acknowledgement": invalid,
			}}, repair: map[string]any{"acknowledgement": "I’ll check which projects are available."}}
			req := compileReq()
			req.Statement = "Which projects are available?"
			req.Input = map[string]any{"conversation": map[string]any{"messages": []any{}}}
			out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			require.NoError(t, err)
			require.Equal(t, "lookup", out.Workload)
			require.Equal(t, "Check available projects", out.WorkTitle)
			require.Equal(t, "I’ll check which projects are available.", out.Acknowledgement)
			require.Equal(t, []string{"goalComplexityTriage", "workAcknowledgement"}, engine.aiCalls)
			require.Equal(t, 2, out.ModelCalls)
			require.Equal(t, req.Statement, engine.aiData[1]["goal"])
			require.WithinDuration(t, time.Now().Add(15*time.Second), engine.deadlines[1], time.Second)
		})
	}
}

func fmtName(value string) string {
	if len(value) > 20 {
		return "oversized"
	}
	if value == "" {
		return "empty"
	}
	return "whitespace"
}

func TestAcknowledgementFailureNeverLoopsOrManufacturesProse(t *testing.T) {
	for _, test := range []struct {
		name, workload string
		maxCalls       int
		repair         any
		err            error
		calls          int
	}{
		{"invalid repair", "lookup", 0, map[string]any{"acknowledgement": ""}, nil, 2},
		{"provider error", "research", 0, nil, errors.New("provider failed"), 2},
		{"budget exhausted", "project", 1, nil, nil, 1},
		{"quick answer", "quick", 0, nil, nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &acknowledgementTestEngine{countingCompileEngine: &countingCompileEngine{triage: map[string]any{
				"complexity": "trivial", "intent": "reply", "requiresFile": false, "workload": test.workload,
			}}, repair: test.repair, repairErr: test.err}
			req := compileReq()
			req.MaxModelCalls = test.maxCalls
			req.Input = map[string]any{"conversation": map[string]any{"messages": []any{}}}
			out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			require.NoError(t, err)
			require.Empty(t, out.Acknowledgement)
			require.Len(t, engine.aiCalls, test.calls)
		})
	}
}
