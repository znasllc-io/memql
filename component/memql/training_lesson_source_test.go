package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestLessonAndEvaluationReceiptsCannotBeForgedOrRewritten(t *testing.T) {
	for _, key := range []string{"trainingLesson", "goalEvaluation"} {
		row := map[string]any{"data": map[string]any{key: map[string]any{"schemaVersion": 1, "enabled": true}}}
		guard, with := guardTrainingLessonEvent, ContextWithTrainingLessonWrite
		if key == "goalEvaluation" {
			guard, with = guardGoalEvaluationEvent, ContextWithGoalEvaluationWrite
		}
		require.Error(t, guard(context.Background(), nil, row))
		require.Error(t, guard(auth.ContextWithInternalOrigin(context.Background()), nil, row), "internal origin alone is not the writer capability")
		require.NoError(t, guard(with(context.Background()), nil, row))
		require.Error(t, guard(with(context.Background()), row, row), "even native writes cannot amend a receipt")
		require.Error(t, guard(context.Background(), row, map[string]any{"deleted": true}))
	}
}

// A routed message is coaching for the goal its work guidance reached: the
// source names that run and that delivery's status. An answer given through
// a request, like ordinary chat, is not a lesson source.
func TestManualLessonSourceReadsSavedGuidanceAcrossReplicas(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	waiting := guidanceRootRun(write, conversation, "waiting-goal", "waiting-plan", "waiting")
	quote := "Distinguish worker policy from task permission."
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "lesson-feedback", Prompt: quote, Routed: &AskRouting{Targets: []AskRouteTarget{
		{GoalID: "waiting-goal", RunID: waiting, ApprovalID: "scope-question", Status: routeNotApproved, Text: quote},
		{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Epoch: guidanceEpochFor(guidanceRun(t, a, ctx, run)), Text: quote},
	}}})
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "request-answer", Prompt: "Use the saved one.", Routed: &AskRouting{Targets: []AskRouteTarget{
		{GoalID: "waiting-goal", RunID: waiting, ApprovalID: "feedback-question", Status: routeAnswered, Text: "Use the saved one."},
	}}})
	source, err := b.TrainingLessonSource(ctx, conversation, "lesson-feedback")
	require.NoError(t, err)
	require.Equal(t, quote, source["quote"])
	require.Equal(t, BareShortId(run), source["runId"])
	require.Equal(t, "queued", source["status"])
	_, err = b.TrainingLessonSource(auth.ContextWithUserActor(ctx, "another-owner"), conversation, "lesson-feedback")
	require.Error(t, err)
	_, err = b.TrainingLessonSource(ctx, conversation, "original")
	require.Error(t, err, "ordinary chat text is not coaching evidence")
	_, err = b.TrainingLessonSource(ctx, conversation, "request-answer")
	require.ErrorContains(t, err, "saved coaching message is unavailable", "an answer to a request is not work guidance")
}

func TestTrainingEvidenceRequiresNativeRecorder(t *testing.T) {
	for _, data := range []map[string]any{{"trainingSelection": map[string]any{}}, {"execution": map[string]any{"call": map[string]any{"phase": "completed"}}}} {
		row := map[string]any{"data": data}
		require.Error(t, guardTrainingEvidence(context.Background(), nil, row))
		require.Error(t, guardTrainingEvidence(auth.ContextWithInternalOrigin(context.Background()), nil, row))
		require.Error(t, guardTrainingEvidence(context.Background(), row, map[string]any{"deleted": true}))
		require.NoError(t, guardTrainingEvidence(ContextWithTrainingEvidenceWrite(context.Background()), row, row))
	}
}
