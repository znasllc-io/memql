package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
)

type trainingLessonWriteKey struct{}

// ContextWithTrainingLessonWrite is a native-only receipt writer capability.
// A DSL argument, internal-origin flag or cluster-owner role cannot mint it.
func ContextWithTrainingLessonWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, trainingLessonWriteKey{}, true)
}

func guardTrainingLessonEvent(ctx context.Context, prior, final map[string]any) error {
	has := func(p map[string]any) bool { d, _ := p["data"].(map[string]any); return d["trainingLesson"] != nil }
	if !has(prior) && !has(final) {
		return nil
	}
	if has(prior) || ctx.Value(trainingLessonWriteKey{}) != true {
		return fmt.Errorf("Training lesson events are immutable and require the native owner review path")
	}
	return nil
}

// TrainingLessonSource returns only a person's saved guidance and public run identity.
func (e *MemQLEngine) TrainingLessonSource(ctx context.Context, conversationID, turnID string) (map[string]any, error) {
	row, err := e.askRead(ContextWithFreshRead(ctx), conversationID)
	if err != nil {
		return nil, err
	}
	access, ok := auth.AccessFromContext(ctx)
	// askConversationById has an explicit owner predicate; its detail shape omits ownerUserId.
	if !ok || access == nil || access.UserId == "" {
		return nil, fmt.Errorf("guidance belongs to another owner")
	}
	transcript, err := approvalTranscript(row)
	if err != nil {
		return nil, err
	}
	for _, turn := range transcript.Turns {
		if turn.ID != turnID || turn.Routed == nil {
			continue
		}
		// Coaching is work guidance: a target with no approval. An answer given
		// through a request belongs to that request, not to a lesson.
		for _, target := range turn.Routed.Targets {
			if target.ApprovalID == "" {
				return map[string]any{"conversationId": BareShortId(conversationID), "turnId": turn.ID, "runId": BareShortId(target.RunID), "quote": turn.Prompt, "at": turn.StartedAt, "status": target.Status}, nil
			}
		}
	}
	return nil, fmt.Errorf("saved coaching message is unavailable")
}

// AppendTrainingLessonEvent preserves the caller and bypasses only the protected-field gate.
func (e *MemQLEngine) AppendTrainingLessonEvent(ctx context.Context, args map[string]any) error {
	call, err := parser.RenderCall("createWorkObservation", args)
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ContextWithTrainingLessonWrite(ctx)), "mutation "+call)
	return err
}

type goalEvaluationWriteKey struct{}

// ContextWithGoalEvaluationWrite is held only by the verified evaluator publisher.
func ContextWithGoalEvaluationWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, goalEvaluationWriteKey{}, true)
}
func guardGoalEvaluationEvent(ctx context.Context, prior, final map[string]any) error {
	has := func(p map[string]any) bool { d, _ := p["data"].(map[string]any); return d["goalEvaluation"] != nil }
	if !has(prior) && !has(final) {
		return nil
	}
	if has(prior) || ctx.Value(goalEvaluationWriteKey{}) != true {
		return fmt.Errorf("goal evaluation evidence requires the verified publisher and is immutable")
	}
	return nil
}

type trainingEvidenceWriteKey struct{}

func ContextWithTrainingEvidenceWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, trainingEvidenceWriteKey{}, true)
}
func guardTrainingEvidence(ctx context.Context, prior, final map[string]any) error {
	has := func(p map[string]any) bool {
		d, _ := p["data"].(map[string]any)
		e, _ := d["execution"].(map[string]any)
		return d["trainingSelection"] != nil || d["providerAdmission"] != nil || d["nativeResultEvidence"] != nil || d["progressCredit"] != nil || d["capabilityEvidence"] != nil || e["call"] != nil
	}
	if (has(prior) || has(final)) && ctx.Value(trainingEvidenceWriteKey{}) != true {
		return fmt.Errorf("Training selection and provider delivery receipts require the native execution recorder")
	}
	if d, _ := prior["data"].(map[string]any); d["providerAdmission"] != nil {
		return fmt.Errorf("provider admission receipts are immutable")
	}
	if d, _ := prior["data"].(map[string]any); d["progressCredit"] != nil {
		return fmt.Errorf("progress credit receipts are immutable")
	}
	return nil
}
