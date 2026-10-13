package memql

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// SaveWorkContinuation is a barrier before asking a person. It keeps the exact
// completed tool exchanges; resumption does not reconstruct effects from prose.
func (e *MemQLEngine) SaveWorkContinuation(ctx context.Context, messages []common.ChatMessage) error {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return fmt.Errorf("continuation requires owned work")
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	// Append a receipt rather than read-merging the run row. Heartbeats,
	// cancellation and answers can arrive on another replica during this write.
	call, err := parser.RenderCall("createWorkObservation", map[string]any{
		"observationId": continuationID(run, raw),
		"runId":         run.RunId, "stepKey": run.StepKey, "kind": "note", "content": "Saved execution progress",
		// Tool output may contain NUL, which JSON allows but PostgreSQL JSONB
		// cannot represent in a string. Encode the complete exchange rather than
		// strip bytes or change tool arguments/results (and their receipt hash).
		"data": map[string]any{"continuation": true, "rerunRequestId": run.RerunRequestId, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

func (e *MemQLEngine) RestoreWorkContinuation(ctx context.Context, messages []common.ChatMessage) ([]common.ChatMessage, error) {
	run, ok := common.RunFromContext(ctx)
	if ok && run.Continuation != nil {
		if run.Continuation.StepKey != run.StepKey || !run.Override.Empty() || run.Mode == common.RunModeReplay {
			return nil, fmt.Errorf("work continuation does not match this execution")
		}
		if _, err := e.PrepareWorkContinuation(ctx, CheckpointResumeRequest{RunID: run.RunId, StepKey: run.StepKey, ApprovalID: run.Continuation.ApprovalID, RerunRequestID: run.RerunRequestId}); err != nil {
			return nil, err
		}
	}
	if !ok || run.GoalId == "" || run.Mode == common.RunModeReplay || !run.Override.Empty() {
		return messages, nil
	}
	rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run.RunId)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("work continuation is unavailable")
	}
	call, err := parser.RenderCall("workContinuationForOwnerRun", map[string]any{"runId": run.RunId, "stepKey": run.StepKey})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	checkpoints := MaterializeRows(result.OutputPayload())
	if len(checkpoints) == 0 {
		if run.Continuation != nil {
			return nil, fmt.Errorf("the required work checkpoint is missing")
		}
		return messages, nil
	}
	if run.Continuation != nil && BareShortId(stringField(checkpoints[0], "id")) != BareShortId(run.Continuation.ObservationID) {
		return nil, fmt.Errorf("the required work checkpoint changed")
	}
	saved, _ := checkpoints[0]["data"].(map[string]any)
	if stringField(saved, "rerunRequestId") != run.RerunRequestId {
		if run.Continuation != nil {
			return nil, fmt.Errorf("the required work checkpoint belongs to another execution request")
		}
		return messages, nil
	}
	prior, err := decodeWorkContinuation(saved)
	if err != nil {
		return nil, err
	}
	if len(prior) == 0 {
		if run.Continuation != nil {
			return nil, fmt.Errorf("the required work checkpoint is empty")
		}
		return messages, nil
	}
	if run.Continuation != nil {
		canonical, err := json.Marshal(prior)
		if err != nil || BareShortId(run.Continuation.ObservationID) != continuationID(run, canonical) {
			return nil, fmt.Errorf("the required work checkpoint content changed")
		}
	}
	// Refresh instructions/profile context rather than resurrecting the system
	// prompt captured by a previous replica or before a profile change.
	if len(messages) > 0 && messages[0].Role == "system" && prior[0].Role == "system" {
		prior[0] = messages[0]
	}
	// Keep current instructions and the person's newly recorded answers. The
	// saved tail already contains the original request and completed results.
	for _, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this ") {
			found := false
			for _, previous := range prior {
				if previous.Role == m.Role && previous.Content == m.Content {
					found = true
					break
				}
			}
			if !found {
				prior = append(prior, m)
			}
		}
	}
	return prior, nil
}

func continuationID(run common.RunContext, raw []byte) string {
	prefix := []byte(run.StepKey)
	if run.RerunRequestId != "" {
		// A fresh rerun cannot overwrite an earlier identical transcript or borrow
		// its approval. Preserve the identity of existing ordinary checkpoints.
		prefix, _ = json.Marshal([]string{"rerun-v1", run.StepKey, run.RerunRequestId})
	}
	return fmt.Sprintf("continuation-%s-%x", BareShortId(run.RunId), sha256.Sum256(append(prefix, raw...)))
}

func decodeWorkContinuation(saved map[string]any) ([]common.ChatMessage, error) {
	var raw []byte
	var err error
	switch saved["messagesEncoding"] {
	case nil:
		// Existing immutable receipts contain the array directly. Keep them
		// resumable without rewriting their content-addressed identity.
		raw, err = json.Marshal(saved["messages"])
	case "json-base64-v1":
		encoded, ok := saved["messages"].(string)
		if !ok {
			return nil, fmt.Errorf("invalid encoded work checkpoint")
		}
		raw, err = base64.StdEncoding.DecodeString(encoded)
	default:
		return nil, fmt.Errorf("unsupported work checkpoint encoding")
	}
	if err != nil {
		return nil, fmt.Errorf("invalid work checkpoint encoding")
	}
	var messages []common.ChatMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, fmt.Errorf("invalid work checkpoint messages")
	}
	return messages, nil
}

// PrepareWorkContinuation proves an answered suspension belongs to this owner,
// run and step, then binds the continuation to one content-addressed receipt.
// RestoreWorkContinuation rechecks the proof before a model or tool can run.
func (e *MemQLEngine) PrepareWorkContinuation(ctx context.Context, request CheckpointResumeRequest) (context.Context, error) {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || run.GoalId == "" || run.OwnerUserId == "" || ac == nil ||
		BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) || BareShortId(run.RunId) != BareShortId(request.RunID) ||
		request.StepKey == "" || request.ApprovalID == "" || request.RerunRequestID != run.RerunRequestId || !run.Override.Empty() || run.Mode == common.RunModeReplay {
		return nil, fmt.Errorf("continuation requires the unchanged owned work execution")
	}
	ctx = ContextWithFreshRead(ctx)
	rows, err := e.workRows(ctx, "workRunForOwner", run.RunId)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || rows[0]["cancelRequested"] == true || stringField(rows[0], "status") != "running" ||
		BareShortId(stringField(rows[0], "goalId")) != BareShortId(run.GoalId) ||
		BareShortId(stringField(rows[0], "humanResumeId")) != BareShortId(request.ApprovalID) {
		return nil, fmt.Errorf("the run is not continuing from this human decision")
	}
	storedRerun, _ := rows[0]["rerun"].(map[string]any)
	if stringField(storedRerun, "requestId") != request.RerunRequestID {
		return nil, fmt.Errorf("the execution request changed before continuation")
	}
	read := func(name string, args map[string]any) ([]map[string]any, error) {
		call, err := parser.RenderCall(name, args)
		if err != nil {
			return nil, err
		}
		result, err := e.Execute(ctx, "query "+call)
		if err != nil {
			return nil, err
		}
		return MaterializeRows(result.OutputPayload()), nil
	}
	approvals, err := read("workApprovalForOwner", map[string]any{"approvalId": request.ApprovalID})
	if err != nil {
		return nil, err
	}
	if len(approvals) != 1 {
		return nil, fmt.Errorf("the human decision is unavailable")
	}
	a := approvals[0]
	decision, kind := stringField(a, "decision"), stringField(a, "kind")
	if BareShortId(stringField(a, "runId")) != BareShortId(run.RunId) || stringField(a, "stepKey") != request.StepKey ||
		!((kind == "feedback" && decision == "answered") || (kind == "scopeElevation" && decision == "approved") || (kind == "budget" && decision == "approved")) {
		return nil, fmt.Errorf("the human decision does not authorize this continuation")
	}
	checkpoints, err := read("workContinuationForOwnerRun", map[string]any{"runId": run.RunId, "stepKey": request.StepKey})
	if err != nil {
		return nil, err
	}
	if len(checkpoints) != 1 {
		return nil, fmt.Errorf("the saved work checkpoint is unavailable")
	}
	saved, _ := checkpoints[0]["data"].(map[string]any)
	if stringField(saved, "rerunRequestId") != request.RerunRequestID {
		return nil, fmt.Errorf("the saved checkpoint belongs to another execution request")
	}
	messages, err := decodeWorkContinuation(saved)
	if err != nil || len(messages) == 0 {
		return nil, fmt.Errorf("the saved work checkpoint is invalid")
	}
	run.StepKey = request.StepKey
	canonical, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	checkpointID := BareShortId(stringField(checkpoints[0], "id"))
	if kind == "budget" {
		subject, _ := a["subject"].(map[string]any)
		if stringField(subject, "resumeKind") != "checkpoint" || BareShortId(stringField(subject, "continuationId")) != checkpointID {
			return nil, fmt.Errorf("the budget decision does not name this saved checkpoint")
		}
	}
	if checkpointID != continuationID(run, canonical) {
		return nil, fmt.Errorf("the saved work checkpoint content changed")
	}
	run.Continuation = &common.StepContinuation{StepKey: request.StepKey, ObservationID: checkpointID, ApprovalID: request.ApprovalID}
	return common.ContextWithRun(ctx, run), nil
}

// PrepareWorkTool revises only a classifier estimate, through the work
// integration's durable writer. It never grants permission or changes a goal.
func (e *MemQLEngine) PrepareWorkTool(ctx context.Context) error {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return fmt.Errorf("workload transition requires owned work")
	}
	writer, ok := e.IntegrationByName("work").(interface {
		PrepareWorkTool(context.Context) (bool, error)
	})
	if !ok {
		return fmt.Errorf("workload coordination is unavailable")
	}
	_, err := writer.PrepareWorkTool(ctx)
	return err
}
