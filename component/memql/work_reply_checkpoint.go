package memql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// WorkReplyReviewBinding identifies execution and instruction versions, not
// permission. A separate receipt survives helper checkpoints that replace the
// ordinary model context while a completed operational result awaits review.
type WorkReplyReviewBinding struct {
	Generation string `json:"generation"`
	Guidance   string `json:"guidance"`
}

type WorkReplyReviewCheckpoint struct {
	Binding WorkReplyReviewBinding
	Saved   WorkReplyReviewBinding
	Pending json.RawMessage
}

type replyReviewSourceKey struct{}

// ContextWithWorkReplyReviewSource keeps helper capacity recovery anchored to
// the operational source. A review helper may compact its own prompt, but its
// suspension must not replace the checkpoint needed by later owner guidance.
func ContextWithWorkReplyReviewSource(ctx context.Context, messages []common.ChatMessage) context.Context {
	return context.WithValue(ctx, replyReviewSourceKey{}, append([]common.ChatMessage(nil), messages...))
}

func (e *MemQLEngine) workReplyReviewBinding(ctx context.Context) (WorkReplyReviewBinding, error) {
	if err := ctx.Err(); err != nil {
		return WorkReplyReviewBinding{}, err
	}
	run, ok := common.RunFromContext(ctx)
	actor, _ := auth.AccessFromContext(ctx)
	if !ok || !run.IsRun() || run.Mode == common.RunModeReplay || run.GoalId == "" || run.StepKey == "" || actor == nil || BareShortId(actor.UserId) != BareShortId(run.OwnerUserId) {
		return WorkReplyReviewBinding{}, fmt.Errorf("reply checkpoint requires owned live work")
	}
	rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run.RunId)
	if err != nil || len(rows) != 1 {
		return WorkReplyReviewBinding{}, fmt.Errorf("reply checkpoint run unavailable: %v", err)
	}
	row := rows[0]
	rerun, _ := row["rerun"].(map[string]any)
	if row["status"] != "running" || row["cancelRequested"] == true || BareShortId(stringArg(row, "goalId")) != BareShortId(run.GoalId) || stringArg(rerun, "requestId") != run.RerunRequestId {
		return WorkReplyReviewBinding{}, fmt.Errorf("reply checkpoint execution changed or stopped")
	}
	if stringArg(rerun, "stepKey") == run.StepKey {
		recorded, _ := rerun["override"].(map[string]any)
		if !common.SameStepOverride(common.StepOverrideFromRecord(recorded), run.Override) {
			return WorkReplyReviewBinding{}, fmt.Errorf("reply checkpoint execution override changed")
		}
	}
	goals, err := e.memoryCall(ContextWithFreshRead(ctx), "query", "work.workGoalForOwner", map[string]any{"goalId": run.GoalId})
	if err != nil || len(goals) != 1 || goals[0]["status"] == "closed" {
		return WorkReplyReviewBinding{}, fmt.Errorf("reply checkpoint goal unavailable or closed: %v", err)
	}
	override, err := common.StepOverrideFingerprint(run.Override)
	if err != nil {
		return WorkReplyReviewBinding{}, err
	}
	guidance, err := e.readWorkGuidance(ContextWithFreshRead(ctx), true)
	if err != nil {
		return WorkReplyReviewBinding{}, err
	}
	generation, err := json.Marshal([]any{BareShortId(run.RunId), BareShortId(run.GoalId), BareShortId(run.OwnerUserId), run.StepKey, run.RerunRequestId, override, row["templateFingerprint"]})
	if err != nil {
		return WorkReplyReviewBinding{}, err
	}
	direction, err := json.Marshal([]any{goals[0]["statement"], row["input"], guidance})
	if err != nil {
		return WorkReplyReviewBinding{}, err
	}
	return WorkReplyReviewBinding{Generation: workContextHash("reply-generation-v1", generation), Guidance: workContextHash("reply-guidance-v1", direction)}, nil
}

// ReadWorkReplyReview never returns an earlier generation's draft. An invalid
// record is a storage failure, not absence that permits repeating production.
func (e *MemQLEngine) ReadWorkReplyReview(ctx context.Context) (WorkReplyReviewCheckpoint, error) {
	binding, err := e.workReplyReviewBinding(ctx)
	if err != nil {
		return WorkReplyReviewCheckpoint{}, err
	}
	state := WorkReplyReviewCheckpoint{Binding: binding}
	run, _ := common.RunFromContext(ctx)
	call, err := parser.RenderCall("workReplyReviewForOwnerStep", map[string]any{"runId": run.RunId, "stepKey": run.StepKey})
	if err != nil {
		return state, err
	}
	result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return state, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) == 0 {
		return state, nil
	}
	data, _ := rows[0]["data"].(map[string]any)
	var saved struct {
		Version  int                    `json:"version"`
		Binding  WorkReplyReviewBinding `json:"binding"`
		Pending  bool                   `json:"pending"`
		Encoding string                 `json:"encoding"`
		Value    string                 `json:"value"`
		Digest   string                 `json:"digest"`
	}
	raw, marshalErr := json.Marshal(data["replyReviewStage"])
	if len(rows) != 1 || marshalErr != nil || json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || len(saved.Binding.Generation) != 64 || len(saved.Binding.Guidance) != 64 || saved.Encoding != "json-base64-v1" {
		return state, fmt.Errorf("unreadable reply review checkpoint")
	}
	value, err := base64.StdEncoding.DecodeString(saved.Value)
	if err != nil || workContextHash("reply-review-v1", value) != saved.Digest || (saved.Pending && !json.Valid(value)) || (!saved.Pending && len(value) != 0) {
		return state, fmt.Errorf("reply review checkpoint content changed")
	}
	if saved.Binding.Generation == binding.Generation && saved.Pending {
		state.Saved, state.Pending = saved.Binding, value
	}
	return state, nil
}

// SaveWorkReplyReview is a write barrier before the review call. Guidance may
// change while production runs: retain its original binding and let the reader
// require fresh production with the completed receipts preserved. A generation
// change or cancellation refuses the write. Nil value retires a pending draft.
func (e *MemQLEngine) SaveWorkReplyReview(ctx context.Context, binding WorkReplyReviewBinding, value json.RawMessage) error {
	current, err := e.workReplyReviewBinding(ctx)
	if err != nil {
		return err
	}
	if current.Generation != binding.Generation || len(binding.Guidance) != 64 || (len(value) > 0 && !json.Valid(value)) {
		return fmt.Errorf("reply review checkpoint binding changed")
	}
	run, _ := common.RunFromContext(ctx)
	data := map[string]any{"version": 1, "binding": binding, "pending": len(value) > 0, "encoding": "json-base64-v1", "value": base64.StdEncoding.EncodeToString(value), "digest": workContextHash("reply-review-v1", value)}
	// Each transition has its own receipt. In particular, retiring two drafts
	// with the same binding must not reuse the first retirement's storage row.
	call, err := parser.RenderCall("createWorkObservation", map[string]any{"observationId": id.NewShortId(), "runId": run.RunId, "stepKey": run.StepKey, "kind": "note", "content": "Saved reply review stage", "data": map[string]any{"replyReviewStage": data}})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}
