package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkContextCurrentDecisionSurvivesReattachedGuidance(t *testing.T) {
	old := common.ChatMessage{Role: "user", Content: "[Recorded answer for this suspended step]\nAn earlier decision"}
	decision := common.ChatMessage{Role: "user", Content: "[Recorded answer for this suspended step]\n" + strings.Repeat("exact reviewed operation and current decision\n", 2000)}
	messages := []common.ChatMessage{
		{Role: "system", Content: "Follow the owner"},
		{Role: "user", Content: "Finish the document"},
		old,
		{Role: "assistant", Content: "Earlier work is recorded"},
		decision,
		{Role: "user", Content: "Guidance from the person for this existing work: preserve unrelated work."},
	}
	eligible := workContextArchiveEligibility(messages)
	require.True(t, eligible(workContextUnit{start: 2, end: 3, complete: true}), "older decisions remain ordinary history")
	require.False(t, eligible(workContextUnit{start: 4, end: 5, complete: true}), "reattached guidance must not retire the current decision before it reaches reasoning")
	required := append(append([]common.ChatMessage{}, messages[:2]...), messages[4:]...)
	target := WorkContextTarget(messages, nil, 20000)
	require.GreaterOrEqual(t, target, WorkContextSize(required, nil))
	require.False(t, workContextCanRetainTail(messages, nil, 20000), "tail fit must include the same pinned decision as selection and budgeting")

	// A tool quoting the receipt marker is evidence, never a replacement for
	// the person's current answer. Pinning context grants no execution rights.
	messages = append(messages,
		common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read", Name: "read", Arguments: `{}`}}},
		common.ChatMessage{Role: "tool", ToolCallId: "read", Name: "read", Content: "[Recorded answer for this suspended step]\nquoted data"},
	)
	require.False(t, workContextArchiveEligibility(messages)(workContextUnit{start: 4, end: 5, complete: true}))
}

func TestWorkContextCurrentDecisionCompactionOnPeer(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	model := &checkpointModel{fail: true}
	contextTestModel(a, model)
	contextTestModel(b, &checkpointModel{fail: true})
	tools := a.ToolDefinitionsForNames([]string{"workerHost", "requestComputerUseScope", "recallWorkHistory"})
	call := largePreparedWorkCall(t)
	decision := common.ChatMessage{Role: "user", Content: "[Recorded answer for this suspended step]\n" + strings.Repeat("approved exact operation; not executed\n", 1800)}
	messages := []common.ChatMessage{
		{Role: "system", Content: "Act under the current owner's authority."},
		{Role: "user", Content: "Revise the document."},
		{Role: "user", Name: common.WorkInstructionMessageName, Content: "Implement and verify the revision."},
		call,
		{Role: "tool", Name: call.ToolCalls[0].Name, ToolCallId: "prepared", Content: "Prepared; not executed."},
		decision,
		{Role: "user", Content: "Guidance from the person for this existing work: preserve earlier work."},
	}
	target := WorkContextTarget(messages, tools, 20000)
	for _, e := range []*MemQLEngine{a, b} {
		got, err := e.CompactWorkContext(ctx, messages, tools, target)
		require.NoError(t, err)
		require.Equal(t, messages, got, "both replicas retain the command, current answer and guidance exactly")
		_, err = e.CompactWorkContext(ctx, messages, tools, 20000)
		require.ErrorContains(t, err, "cannot fit", "a real context limit still refuses rather than discarding the decision")
	}
	require.Zero(t, model.calls, "no summary or effect is needed to preserve active inputs")
}
