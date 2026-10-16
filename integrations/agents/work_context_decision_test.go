package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type decisionContextReader struct {
	run      map[string]any
	decision map[string]any
	fail     bool
}

func (r *decisionContextReader) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	switch {
	case strings.HasPrefix(q, "query workRunForOwner("):
		return memql.NewResultWithOutput([]map[string]any{r.run}), nil
	case strings.HasPrefix(q, "query workQuestionsForOwnerRun("):
		return memql.NewResultWithOutput([]map[string]any{}), nil
	case strings.HasPrefix(q, "query workApprovalForOwner("):
		if r.fail {
			return nil, errors.New("decision read failed")
		}
		if r.decision == nil {
			return memql.NewResultWithOutput([]map[string]any{}), nil
		}
		return memql.NewResultWithOutput([]map[string]any{r.decision}), nil
	default:
		return nil, errors.New("unexpected query: " + q)
	}
}

func TestWorkTurnHistoryProjectsOnlyItsExactNativeDecision(t *testing.T) {
	for _, kind := range []string{"scopeElevation", "budget"} {
		t.Run(kind, func(t *testing.T) {
			run := common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "inspect", Mode: common.RunModeLive,
				Continuation: &common.StepContinuation{ApprovalID: "approval", StepKey: "inspect", ObservationID: "checkpoint"}}
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), run)
			reader := &decisionContextReader{
				run: map[string]any{"humanResumeId": "approval"},
				decision: map[string]any{"id": "approval", "runId": "run", "stepKey": "inspect", "kind": kind, "decision": "approved",
					"question": "Inspect the selected repository?", "subject": map[string]any{"command": "git status --short", "cwd": "/work/repo", "timeoutSeconds": 60},
					"answer": map[string]any{"permissionMode": "once"}, "decidedAt": "2026-10-13T08:26:00Z"},
			}
			history, err := workTurnHistory(ctx, reader, "Continue the saved work")
			require.NoError(t, err)
			require.Len(t, history, 3, "the approval must reach the next model request, not just the dispatcher")
			require.Equal(t, "Continue the saved work", history[0].Content)
			require.Equal(t, "user", history[2].Role)
			require.True(t, strings.HasPrefix(history[2].Content, "[Recorded answer for this "), "the saved checkpoint restorer must retain this receipt")
			require.Contains(t, history[2].Content, "does not grant additional permission or prove an operation executed")
			var receipt map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.SplitN(history[2].Content, "\n", 2)[1]), &receipt))
			require.Equal(t, "approved", receipt["decision"])
			require.Equal(t, kind, receipt["kind"])
			require.Equal(t, "git status --short", receipt["subject"].(map[string]any)["command"])
			require.Equal(t, "once", receipt["answer"].(map[string]any)["permissionMode"])
			// A saved receipt must not hide that its use was consumed or revoked.
			reader.decision["permissionUsedAt"] = "used"
			reader.decision["permissionRevokedAt"] = "revoked"
			history, err = workTurnHistory(ctx, reader, "Continue")
			require.NoError(t, err)
			require.Contains(t, history[2].Content, `"permissionUsedAt":"used"`)
			require.Contains(t, history[2].Content, `"permissionRevokedAt":"revoked"`)
			for _, tc := range []struct{ field, value string }{
				{"id", "other"}, {"runId", "other"}, {"stepKey", "other"}, {"decision", "pending"}, {"decision", "rejected"}, {"kind", "unrelated"},
			} {
				t.Run(tc.field+"="+tc.value, func(t *testing.T) {
					prior := reader.decision[tc.field]
					reader.decision[tc.field] = tc.value
					_, err := workTurnHistory(ctx, reader, "Continue")
					require.Error(t, err)
					reader.decision[tc.field] = prior
				})
			}
			reader.run["humanResumeId"] = "another-answer"
			_, err = workTurnHistory(ctx, reader, "Continue")
			require.Error(t, err)
			reader.run["humanResumeId"] = "approval"
			reader.fail = true
			_, err = workTurnHistory(ctx, reader, "Continue")
			require.ErrorContains(t, err, "decision read failed")
			reader.fail = false
			reader.decision = nil
			_, err = workTurnHistory(ctx, reader, "Continue")
			require.Error(t, err)
			// A normal turn does not infer authority from old decisions in the run.
			run.Continuation = nil
			history, err = workTurnHistory(common.ContextWithRun(ctx, run), reader, "Continue")
			require.NoError(t, err)
			require.Len(t, history, 2)
		})
	}
}
