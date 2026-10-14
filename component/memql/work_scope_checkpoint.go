package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/num"
)

// BindWorkScopeCheckpoint is a native receipt boundary, not a DSL capability.
// Untyped requests outside a saved work turn retain their existing behavior.
// A caller cannot supply this witness through requestComputerUseScope args.
func (e *MemQLEngine) BindWorkScopeCheckpoint(ctx context.Context, args, subject map[string]any) (map[string]any, error) {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || run.GoalId == "" || run.StepKey == "" || !run.Override.Empty() || run.Mode == common.RunModeReplay {
		return subject, nil
	}
	if ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("scope checkpoint requires owned work")
	}
	ctx = ContextWithFreshRead(ctx)
	runs, err := e.workRows(ctx, "workRunForOwner", run.RunId)
	if err != nil {
		return nil, err
	}
	if len(runs) != 1 || runs[0]["cancelRequested"] == true || stringField(runs[0], "status") != "running" || BareShortId(stringField(runs[0], "goalId")) != BareShortId(run.GoalId) {
		return nil, fmt.Errorf("scope checkpoint run is no longer active")
	}
	rerun, _ := runs[0]["rerun"].(map[string]any)
	if stringField(rerun, "requestId") != run.RerunRequestId {
		return nil, fmt.Errorf("scope checkpoint execution request changed")
	}
	call, err := parser.RenderCall("workContinuationForOwnerRun", map[string]any{"runId": run.RunId, "stepKey": run.StepKey})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) == 0 {
		return subject, nil
	}
	data, _ := rows[0]["data"].(map[string]any)
	if stringField(data, "rerunRequestId") != run.RerunRequestId {
		return subject, nil
	}
	messages, err := decodeWorkContinuation(data)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(messages)
	if err != nil || BareShortId(stringField(rows[0], "id")) != continuationID(run, raw) {
		return nil, fmt.Errorf("scope checkpoint content changed")
	}
	pending, savedArgs, ok := pendingScopeCall(messages)
	if !ok || !scopeRequestMatches(run, savedArgs, subject) {
		return subject, nil
	}
	// The adapter adds these actor fields after parsing the original call. All
	// other authored fields must match, including absent vs supplied operation.
	actual := make(map[string]any, len(args))
	for k, v := range args {
		if k != "ownerUserId" && k != "agentId" && k != "partitionId" {
			actual[k] = v
		}
	}
	authored := make(map[string]any, len(savedArgs))
	for k, v := range savedArgs {
		if k != "ownerUserId" && k != "agentId" && k != "partitionId" {
			authored[k] = v
		}
	}
	if work.ArtifactHash(actual) != work.ArtifactHash(authored) {
		return subject, nil
	}
	// Hash the same JSON object shape a fresh replica reads (in particular,
	// requireLabels arrives here as map[string]string, but storage decodes it
	// as map[string]any). This normalization is only for the new native receipt.
	encoded, err := json.Marshal(subject)
	if err != nil {
		return nil, err
	}
	var bound map[string]any
	if err := json.Unmarshal(encoded, &bound); err != nil {
		return nil, err
	}
	bound["nativeScopeCheckpoint"] = map[string]any{"version": 1, "continuationId": stringField(rows[0], "id"), "toolCallId": pending.ID, "requestHash": work.ArtifactHash(savedArgs)}
	return bound, nil
}

func scopeCheckpointWitness(subject map[string]any) (map[string]any, bool) {
	w, ok := subject["nativeScopeCheckpoint"].(map[string]any)
	if !ok || len(w) != 4 || (w["version"] != 1 && w["version"] != float64(1)) || stringField(w, "continuationId") == "" || stringField(w, "toolCallId") == "" || stringField(w, "requestHash") == "" {
		return nil, false
	}
	return w, subject["scope"] == "observe" || subject["scope"] == "full"
}

func scopeCheckpointMatches(run common.RunContext, subject map[string]any, checkpointID string, messages []common.ChatMessage) bool {
	w, ok := scopeCheckpointWitness(subject)
	if !ok || BareShortId(stringField(w, "continuationId")) != BareShortId(checkpointID) {
		return false
	}
	call, args, ok := pendingScopeCall(messages)
	return ok && call.ID == stringField(w, "toolCallId") && work.ArtifactHash(args) == stringField(w, "requestHash") && scopeRequestMatches(run, args, subject)
}

// Only the first unexecuted call in the latest assistant batch can be the
// currently executing question. Later proposed calls and older questions do
// not establish an execution boundary. Require the complete paired protocol.
func pendingScopeCall(messages []common.ChatMessage) (common.ToolCall, map[string]any, bool) {
	start := -1
	for n, m := range messages {
		if m.Role == "assistant" {
			start = n
		}
	}
	if start < 0 || len(messages[start].ToolCalls) == 0 {
		return common.ToolCall{}, nil, false
	}
	results := make(map[string]common.ChatMessage)
	for _, m := range messages[start+1:] {
		if m.Role != "tool" || m.ToolCallId == "" {
			return common.ToolCall{}, nil, false
		}
		if _, exists := results[m.ToolCallId]; exists {
			return common.ToolCall{}, nil, false
		}
		results[m.ToolCallId] = m
	}
	calls := messages[start].ToolCalls
	if len(results) != len(calls) {
		return common.ToolCall{}, nil, false
	}
	var first *common.ToolCall
	for _, c := range calls {
		m, ok := results[c.ID]
		if !ok || m.Name != c.Name {
			return common.ToolCall{}, nil, false
		}
		delete(results, c.ID)
		if first == nil && m.Content == common.UnexecutedWorkToolResult {
			copy := c
			first = &copy
		}
	}
	if first == nil || first.Name != "requestComputerUseScope" {
		return common.ToolCall{}, nil, false
	}
	var args map[string]any
	if json.Unmarshal([]byte(first.Arguments), &args) != nil || args == nil {
		return common.ToolCall{}, nil, false
	}
	if _, operation := args["operation"]; operation {
		return common.ToolCall{}, nil, false
	}
	return *first, args, true
}

func scopeRequestMatches(run common.RunContext, args, subject map[string]any) bool {
	if _, typed := subject["computerPermission"]; typed {
		return false
	}
	for key, expected := range map[string]string{"ownerUserId": run.OwnerUserId, "agentId": stringField(subject, "agentId")} {
		if supplied, ok := args[key]; ok && BareShortId(fmt.Sprint(supplied)) != BareShortId(expected) {
			return false
		}
	}
	scope := strings.TrimSpace(stringField(args, "requestedScope"))
	if scope == "interact" {
		scope = "full"
	}
	summary := strings.TrimSpace(stringField(args, "summary"))
	if target := stringField(subject, "target"); target != "" {
		summary = strings.TrimRight(summary, " .") + ". Runs " + target + "."
	}
	labels := map[string]string{}
	if values, ok := args["requireLabels"].(map[string]any); ok {
		for k, v := range values {
			if k = strings.TrimSpace(k); k == "" || v == nil {
				continue
			}
			if n, ok := v.(float64); ok {
				if whole, ok := num.WholeInt64(n); ok {
					labels[k] = fmt.Sprintf("%d", whole)
					continue
				}
			}
			labels[k] = fmt.Sprint(v)
		}
	}
	storedLabels := map[string]string{}
	raw, err := json.Marshal(subject["requireLabels"])
	if err != nil || json.Unmarshal(raw, &storedLabels) != nil {
		return false
	}
	if len(storedLabels) == 0 {
		storedLabels = map[string]string{}
	}
	return (scope == "observe" || scope == "full") && subject["scope"] == scope && strings.TrimSpace(stringField(args, "intent")) != "" && subject["intent"] == strings.TrimSpace(stringField(args, "intent")) && summary != "" && subject["summary"] == summary && reflect.DeepEqual(labels, storedLabels)
}
