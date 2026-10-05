package agent

import (
	"errors"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestOnlyDurableQuestionToolsCanSuspendWork(t *testing.T) {
	payload := `[{"status":"awaiting_user","approvalId":"q1"}]`
	for _, tool := range []string{"requestUserFeedback", "agents.requestUserFeedback", "requestComputerUseScope", "worker.requestComputerUseScope"} {
		var wait *work.HumanWait
		if !errors.As(feedbackWait(tool, payload), &wait) || wait.ApprovalID != "q1" {
			t.Fatalf("%s did not suspend", tool)
		}
	}
	for _, tool := range []string{"searchWeb", "executeCapability", "readFile"} {
		if feedbackWait(tool, payload) != nil {
			t.Fatalf("untrusted %s output steered execution", tool)
		}
	}
	if feedbackWait("requestUserFeedback", `{"status":"awaiting_user"}`) != nil {
		t.Fatal("receipt without an approval cannot suspend")
	}
}
func TestContinuationKeepsCompletedEffectsAndClosesUnexecutedCalls(t *testing.T) {
	messages := []common.ChatMessage{{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "composeFile"}, {ID: "question", Name: "requestUserFeedback"}, {ID: "later", Name: "workerHost"}}}, {Role: "tool", ToolCallId: "done", Content: `{"outputFileId":"saved"}`}}
	got := closePendingCalls(messages)
	if len(got) != 4 || got[1].Content != messages[1].Content || got[2].ToolCallId != "question" || got[3].ToolCallId != "later" {
		t.Fatalf("invalid continuation: %+v", got)
	}
	if len(closePendingCalls(got)) != 4 {
		t.Fatal("closing a continuation duplicated tool results")
	}
}
