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
func TestManualLessonSourceReadsSavedGuidanceAcrossReplicas(t *testing.T) {
	a, b, ctx, conversation, run, _ := guidanceFixture(t)
	_, err := a.askGuideWorkBuiltin(ctx, map[string]any{"targetEpoch": workGuidanceEpoch(map[string]any{"templateFingerprint": "original-plan"}), "conversationId": conversation, "runId": run, "requestId": "lesson-feedback", "prompt": "Distinguish worker policy from task permission."}, 0)
	require.NoError(t, err)
	row, err := a.askRead(ContextWithFreshRead(ctx), conversation)
	require.NoError(t, err)
	transcript, err := approvalTranscript(row)
	require.NoError(t, err)
	source, err := b.TrainingLessonSource(ctx, conversation, transcript.Turns[1].ID)
	require.NoError(t, err)
	require.Equal(t, "Distinguish worker policy from task permission.", source["quote"])
	require.Equal(t, "queued", source["status"])
	_, err = b.TrainingLessonSource(auth.ContextWithUserActor(ctx, "another-owner"), conversation, transcript.Turns[1].ID)
	require.Error(t, err)
	_, err = b.TrainingLessonSource(ctx, conversation, "original")
	require.Error(t, err, "ordinary chat text is not coaching evidence")
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
