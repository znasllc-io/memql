package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func largePreparedWorkCall(t *testing.T) common.ChatMessage {
	t.Helper()
	args, err := json.Marshal(map[string]any{"command": strings.Repeat("prepared document transformation\n", 2400)})
	require.NoError(t, err)
	return common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "prepared", Name: "executePreparedWork", Arguments: string(args)}}}
}

func TestWorkContextTargetIncludesRequiredCallsButNotCompletedResultPayloads(t *testing.T) {
	pinned := []common.ChatMessage{{Role: "system", Content: "Preserve authority"}, {Role: "user", Content: "Update the document"}}
	call := largePreparedWorkCall(t)
	for _, complete := range []bool{false, true} {
		messages := append(append([]common.ChatMessage{}, pinned...), call)
		if complete {
			messages = append(messages, common.ChatMessage{Role: "tool", Name: call.ToolCalls[0].Name, ToolCallId: "prepared", Content: "Prepared; not executed"})
		}
		messages = append(messages, common.ChatMessage{Role: "user", Content: "Approved"})
		target := WorkContextTarget(messages, nil, 20000)
		require.GreaterOrEqual(t, target, WorkContextSize(messages, nil), "a protected current call cannot be retired to meet a history preference")
		if complete {
			messages[len(messages)-2].Content = strings.Repeat("large historical result ", 50000)
			require.Equal(t, target, WorkContextTarget(messages, nil, 20000), "completed result bodies still use bounded exact archival")
		}
	}
	// Once a later assistant turn consumes the completed call, it is history.
	old := append(append([]common.ChatMessage{}, pinned...), call,
		common.ChatMessage{Role: "tool", Name: call.ToolCalls[0].Name, ToolCallId: "prepared", Content: "Prepared"},
		common.ChatMessage{Role: "assistant", Content: "The recorded operation completed."})
	require.Equal(t, 20000, WorkContextTarget(old, nil, 20000), "older calls must not expand their own history allowance")
	partialCall := call
	partialCall.ToolCalls = append(append([]common.ToolCall{}, call.ToolCalls...), common.ToolCall{ID: "second", Name: "read", Arguments: `{}`})
	partial := append(append([]common.ChatMessage{}, pinned...), partialCall,
		common.ChatMessage{Role: "tool", Name: call.ToolCalls[0].Name, ToolCallId: "prepared", Content: strings.Repeat("already returned evidence ", 2000)})
	require.GreaterOrEqual(t, WorkContextTarget(partial, nil, 20000), WorkContextSize(partial, nil), "an unfinished parallel group must keep its existing results exact")
}

func TestWorkContextPreparedCallSurvivesApprovalWithoutRelaxingHardRecovery(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, e)
	model := &checkpointModel{fail: true}
	contextTestModel(e, model)
	tools := e.ToolDefinitionsForNames([]string{"workerHost", "requestComputerUseScope", "recallWorkHistory"})
	call := largePreparedWorkCall(t)
	messages := []common.ChatMessage{
		{Role: "system", Content: "Act only under the current owner's authority."},
		{Role: "user", Content: "Finish the requested document revision."},
		{Role: "user", Name: common.WorkInstructionMessageName, Content: "Implement the revision and verify it."},
		call,
		{Role: "tool", Name: call.ToolCalls[0].Name, ToolCallId: "prepared", Content: "The exact operation is approved; it has not run."},
		{Role: "user", Content: "Continue the approved work."},
	}
	before := mustMarshalMessages(t, messages)
	target := WorkContextTarget(messages, tools, 20000)
	got, err := e.CompactWorkContext(ctx, messages, tools, target)
	require.NoError(t, err, "the proactive preference must include the operation it cannot legally compact")
	require.Equal(t, messages, got)
	require.Zero(t, model.calls, "no summary or operation replay is needed")
	_, err = e.CompactWorkContext(ctx, messages, tools, 20000)
	require.ErrorContains(t, err, "cannot fit", "actual provider-window recovery keeps its strict target")
	require.Equal(t, before, mustMarshalMessages(t, messages), "approval, call and original input stay exact")
}
