package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func recordApprovalGuidance(write func(string, map[string]any), owner, run, text string) string {
	approval := id.NewShortId()
	write("createWorkApproval", map[string]any{"approvalId": approval, "runId": run, "stepKey": "section", "kind": "scopeElevation", "subject": map[string]any{}, "artifactHash": "fixture", "question": "Inspect the inputs?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("decideWorkApproval", map[string]any{"approvalId": approval, "decision": "answered", "decidedBy": owner, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"feedback": text}})
	return approval
}

// A correction sent instead of granting permission must outlive the next
// approval. The next worker has no previous model context or local cursor.
func TestApprovalGuidanceSurvivesLaterDecisionAndReplica(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	first := recordApprovalGuidance(write, actor.UserId, run, "Preserve the draft and restore the original document. Do not publish it.")
	read, err := b.ReadWorkGuidance(guidanceRunContext(ctx, run))
	require.NoError(t, err)
	require.Len(t, read, 1)
	require.Contains(t, read[0].Prompt, "restore the original document")
	require.NoError(t, b.AcknowledgeWorkGuidance(guidanceRunContext(ctx, run), guidanceIDs(read)))

	later := recordApprovalGuidance(write, actor.UserId, run, "This becomes a permission receipt, not guidance.")
	write("decideWorkApproval", map[string]any{"approvalId": later, "decision": "approved", "decidedBy": actor.UserId, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"permissionMode": "once"}})
	write("updateWorkRun", map[string]any{"runId": run, "rerun": map[string]any{"requestId": "new-attempt"}})
	runtime := common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: "fixture-goal", OwnerUserId: actor.UserId, StepKey: "section", RerunRequestId: "new-attempt"})
	again, err := a.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, read, again)
	require.Equal(t, "approval-guidance-"+first, again[0].ID)

	child := id.NewShortId()
	write("createWorkRun", map[string]any{"runId": child, "goalId": "fixture-goal", "parentRunId": run, "automationName": "child", "templateFingerprint": "child-plan", "triggeredBy": "automation", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	childRead, err := b.ReadWorkGuidance(guidanceRunContext(ctx, child))
	require.NoError(t, err)
	require.Equal(t, read, childRead)

	// Newer routed guidance stays newer even though it comes from a different
	// journal surface. A later approval never supersedes the earlier correction.
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "latest", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: "Keep the restored document open for my review."}}}})
	ordered, err := b.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Equal(t, []string{read[0].ID, "latest"}, guidanceIDs(ordered))
	for _, engine := range []*MemQLEngine{a, b} {
		rows, err := engine.workRows(context.WithoutCancel(ctx), "workQuestionsForOwnerRun", run)
		require.NoError(t, err)
		require.Empty(t, rows, "scope decisions do not become optional questions")
	}
}

func TestApprovalGuidanceBoundsAndOwnership(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	other := guidanceRootRun(write, conversation, "other-goal", "other-plan", "running")
	recordApprovalGuidance(write, actor.UserId, other, "Another goal's instruction.")
	recordApprovalGuidance(write, "another-person", run, "Not the owner's decision.")
	read, err := b.ReadWorkGuidance(guidanceRunContext(ctx, run))
	require.NoError(t, err)
	require.Empty(t, read)
	for n := range maxWorkGuidanceMessages {
		recordApprovalGuidance(write, actor.UserId, run, fmt.Sprintf("Correction %d.", n))
	}
	read, err = a.ReadWorkGuidance(guidanceRunContext(ctx, run))
	require.NoError(t, err)
	require.Len(t, read, maxWorkGuidanceMessages)
	recordApprovalGuidance(write, actor.UserId, run, "Beyond the explicit context bound.")
	_, err = b.ReadWorkGuidance(guidanceRunContext(ctx, run))
	require.ErrorContains(t, err, "saved guidance exceeds its bounded context")
}
