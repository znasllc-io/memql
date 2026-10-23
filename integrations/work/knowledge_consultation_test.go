package work

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

type consultationEngine struct{ *recordingEngine }

func (consultationEngine) IsInstalledSkillBaseline(row map[string]any) bool {
	return row["verifiedFixture"] == true
}

func TestKnowledgeConsultationReportsRetrievalWithoutCertifyingReadiness(t *testing.T) {
	for _, status := range []string{"ranked", "empty-eligible-corpus", "index-unavailable", "embedding-timeout"} {
		t.Run(status, func(t *testing.T) {
			i, e := newTestIntegration(t)
			i.engine = consultationEngine{e}
			ctx := common.ContextWithRun(callerContext("u1"), common.RunContext{RunId: "run", GoalId: "goal", StepKey: "step", OwnerUserId: "u1", Mode: common.RunModeLive})
			e.reply("workRunForOwner", map[string]any{"id": "run", "ownerUserId": "u1", "goalId": "goal", "status": "running"})
			e.reply("workStepForOwnerRun", map[string]any{"status": "running", "version": 1})
			e.reply("workGoalForOwner", map[string]any{"id": "goal", "ownerUserId": "u1", "statement": "Prepare a report"})
			e.reply("activeSkillsFull", map[string]any{"id": "visual", "name": "Visual verification", "active": true, "status": "active", "version": 1, "verifiedFixture": true, "instructions": map[string]any{"body": "Inspect the rendered result.", "selectionTerms": []string{"rendered"}}})
			e.reply("libraryConsultWorkTraining", map[string]any{"receiptId": "training-receipt", "status": status, "passages": []any{}})
			rows, err := i.handleConsultKnowledge(ctx, map[string]any{"need": "Check the rendered result", "context": "A new output format was observed.", "limit": 9999}, 0)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			var result map[string]any
			require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
			require.Equal(t, "not-assessed", result["readiness"])
			require.Equal(t, status, result["training"].(map[string]any)["status"])
			require.Len(t, result["skills"], 1)
			require.Contains(t, result["scope"], "not searched")
			for _, c := range e.recorded() {
				require.Equal(t, "u1", c.Actor)
				if c.Name() == "libraryConsultWorkTraining" {
					require.Equal(t, auth.OriginClient, c.Origin, "the fixed gateway must not elevate subsequent calls")
					require.Len(t, c.Args(t), 1, "caller cannot override native retrieval allocation")
				}
				if c.Name() == "createWorkObservation" {
					require.True(t, c.Origin.IsInternal())
				}
			}
			require.Contains(t, e.summary(), "createWorkObservation")
		})
	}
}

func TestKnowledgeConsultationRejectsUnboundedOrUnownedWorkBeforeRetrieval(t *testing.T) {
	for _, tc := range []struct {
		ctx  context.Context
		args map[string]any
	}{
		{callerContext("u1"), map[string]any{"need": "Need"}},
		{context.Background(), map[string]any{"need": "Need"}},
		{callerContext("u1"), map[string]any{"need": strings.Repeat("x", 1001)}},
		{callerContext("u1"), map[string]any{"need": "Need", "context": strings.Repeat("x", 3001)}},
	} {
		i, e := newTestIntegration(t)
		_, err := i.handleConsultKnowledge(tc.ctx, tc.args, 0)
		require.Error(t, err)
		require.Empty(t, e.recorded())
	}
}
