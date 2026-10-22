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
