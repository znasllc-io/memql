package memql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type delegatedSessionKey struct{}

// ContextWithDelegatedSession tags server-observed receipts with their session.
// It carries provenance only; delegatedScope separately verifies authority.
func ContextWithDelegatedSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, delegatedSessionKey{}, sessionID)
}

// RecordDelegatedToolSource preserves an executed MCP exchange for the same
// source projection used by normal agent turns. It is not a continuation:
// restoring this pair as an agent's entire conversation would lose its context.
// Only server-observed tool results enter here, never the app's claimed answer.
func (e *MemQLEngine) RecordDelegatedToolSource(ctx context.Context, name string, args map[string]any, result string) error {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || !run.IsRun() || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) || ActingAgentIdFromContext(ctx) == "" {
		return fmt.Errorf("delegated source requires an owned run and acting agent")
	}
	callID := "delegated-" + id.NewShortId()
	arguments, err := json.Marshal(args)
	if err != nil {
		return err
	}
	messages, err := json.Marshal([]common.ChatMessage{
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: callID, Name: name, Arguments: string(arguments)}}},
		{Role: "tool", Name: name, ToolCallId: callID, Content: result},
	})
	if err != nil {
		return err
	}
	data := map[string]any{"delegatedEvidence": true}
	if sessionID, _ := ctx.Value(delegatedSessionKey{}).(string); sessionID != "" {
		data["appSessionId"] = sessionID
	}
	// The reader already bounds each preview and excludes secret tools and
	// generated prose. An oversized exchange leaves an explicit missing source,
	// rather than turning truncated bytes into a complete receipt.
	if len(messages) <= 1024*1024 {
		data["messagesEncoding"] = "json-base64-v1"
		data["messages"] = base64.StdEncoding.EncodeToString(messages)
	} else {
		data["sourceOmitted"] = true
	}
	call, err := parser.RenderCall("createWorkObservation", map[string]any{
		"observationId": callID, "runId": run.RunId, "stepKey": run.StepKey,
		"kind": "note", "content": "Delegated tool source receipt", "data": data,
	})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

func (e *MemQLEngine) delegatedSessionRow(ctx context.Context, sessionID string) (map[string]any, error) {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || ac == nil || run.GoalId == "" || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("delegated continuation requires owned work")
	}
	call, err := parser.RenderCall("appSessionById", map[string]any{"sessionId": sessionID})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 || BareShortId(stringArg(rows[0], "ownerUserId")) != BareShortId(run.OwnerUserId) ||
		BareShortId(stringArg(rows[0], "runId")) != BareShortId(run.RunId) || BareShortId(stringArg(rows[0], "stepId")) != BareShortId(run.StepKey) {
		return nil, fmt.Errorf("delegated session does not belong to this work step")
	}
	return rows[0], nil
}

// PrepareDelegatedFeedback preserves the native turn plus all public executed
// exchanges from this app session before a tool can create an approval. The app
// transcript is never used as resumable state or as proof of completed effects.
func (e *MemQLEngine) PrepareDelegatedFeedback(ctx context.Context, sessionID string, initial []common.ChatMessage, tool string, args map[string]any) error {
	if len(initial) == 0 {
		return fmt.Errorf("%w: delegated turn context is unavailable", ErrWorkCheckpointRequired)
	}
	session, err := e.delegatedSessionRow(ctx, sessionID)
	if err != nil {
		return err
	}
	started, valid := askTimestamp(session["startedAt"])
	if !valid {
		return fmt.Errorf("%w: delegated session start is unavailable", ErrWorkCheckpointRequired)
	}
	run, _ := common.RunFromContext(ctx)
	call, _ := parser.RenderCall("workDelegatedSourcesForOwnerRun", map[string]any{"runId": run.RunId})
	var exchanges [][]common.ChatMessage
	cursor, complete, bytes := "", false, 0
	for range 16 {
		result, err := e.Execute(ContextWithCursor(ContextWithFreshRead(ctx), cursor), "query "+call)
		if err != nil {
			return err
		}
		for _, row := range MaterializeRows(result.OutputPayload()) {
			if at, ok := askTimestamp(row["createdAt"]); ok && at.Before(started) {
				complete = true
				break
			}
			data, _ := row["data"].(map[string]any)
			if BareShortId(stringArg(data, "appSessionId")) != BareShortId(sessionID) {
				continue
			}
			if BareShortId(stringArg(row, "stepKey")) != BareShortId(run.StepKey) || data["sourceOmitted"] == true {
				return fmt.Errorf("%w: delegated source receipt is incomplete", ErrWorkCheckpointRequired)
			}
			pair, err := decodeWorkContinuation(data)
			if err != nil || len(pair) != 2 || pair[0].Role != "assistant" || len(pair[0].ToolCalls) != 1 || pair[1].Role != "tool" || pair[1].ToolCallId != pair[0].ToolCalls[0].ID {
				return fmt.Errorf("%w: delegated source receipt is invalid", ErrWorkCheckpointRequired)
			}
			bytes += len(pair[0].ToolCalls[0].Arguments) + len(pair[1].Content)
			if bytes > 4*1024*1024 {
				return fmt.Errorf("%w: delegated continuation exceeds its byte bound", ErrWorkCheckpointRequired)
			}
			exchanges = append(exchanges, pair)
		}
		cursor = ""
		if meta := result.GetMeta(); meta != nil {
			cursor = meta.Cursor
		}
		if cursor == "" || complete {
			complete = true
			break
		}
	}
	if !complete {
		return fmt.Errorf("%w: delegated continuation exceeds its scan bound", ErrWorkCheckpointRequired)
	}
	messages := append([]common.ChatMessage(nil), initial...)
	for i := len(exchanges) - 1; i >= 0; i-- {
		messages = append(messages, exchanges[i]...)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	callID := "delegated-question-" + id.NewShortId()
	messages = append(messages,
		common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: callID, Name: tool, Arguments: string(raw)}}},
		common.ChatMessage{Role: "tool", Name: tool, ToolCallId: callID, Content: common.UnexecutedWorkToolResult})
	return e.SaveWorkContinuation(ctx, messages)
}

// DelegatedWorkHumanWait reads the native control plane after an app returns.
// A question tool already committed its approval and wait; model prose must not
// create a second question or turn the suspended step into a contract failure.
func (e *MemQLEngine) DelegatedWorkHumanWait(ctx context.Context, sessionID string) error {
	session, err := e.delegatedSessionRow(ctx, sessionID)
	if err != nil {
		return err
	}
	run, _ := common.RunFromContext(ctx)
	rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run.RunId)
	if err != nil || len(rows) != 1 {
		return fmt.Errorf("delegated work state is unavailable: %v", err)
	}
	current := rows[0]
	rerun, _ := current["rerun"].(map[string]any)
	if BareShortId(stringArg(current, "ownerUserId")) != BareShortId(run.OwnerUserId) || BareShortId(stringArg(current, "goalId")) != BareShortId(run.GoalId) || current["cancelRequested"] == true || stringArg(rerun, "requestId") != run.RerunRequestId {
		return fmt.Errorf("delegated work execution is no longer current")
	}
	waiting, _ := current["waitingOn"].(map[string]any)
	approvalID := ""
	if current["status"] == "waiting" && waiting["kind"] == "approval" {
		approvalID = stringArg(waiting, "subject")
	} else if current["status"] == "running" {
		// The person can answer while the app is shutting down. Yield the old
		// invocation without overwriting that already-committed resume.
		approvalID = stringArg(current, "humanResumeId")
		if run.Continuation != nil && BareShortId(approvalID) == BareShortId(run.Continuation.ApprovalID) {
			return nil
		}
	}
	if approvalID == "" {
		return nil
	}
	approval, _, err := e.approvalRows(ctx, approvalID)
	if err != nil {
		return err
	}
	created, ok := askTimestamp(approval["requestedAt"])
	started, startOK := askTimestamp(session["startedAt"])
	if !ok || !startOK || created.Before(started) || created.After(time.Now()) || BareShortId(stringArg(approval, "runId")) != BareShortId(run.RunId) || BareShortId(stringArg(approval, "stepKey")) != BareShortId(run.StepKey) {
		return nil
	}
	return &work.HumanWait{ApprovalID: approvalID}
}
