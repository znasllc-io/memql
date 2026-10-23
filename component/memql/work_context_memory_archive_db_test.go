package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkContextArchivesMemoryOnlyHistoryWithoutAnotherSummary(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	model := &checkpointModel{fail: true}
	contextTestModel(a, model)
	source := []common.ChatMessage{{Role: "assistant", Content: "Exact earlier source, including its uncertainty."}}
	raw := mustMarshalMessages(t, source)
	ref := workContextHash("test-source", raw)
	require.NoError(t, a.saveWorkContextSource(ctx, ref, string(raw), ""))
	messages := []common.ChatMessage{
		{Role: "system", Content: "Platform authority"},
		{Role: "user", Name: common.WorkInstructionMessageName, Content: "Finish only the current stage"},
		{Role: "user", Content: "Original request"},
		{Role: "user", Content: "[Memory checkpoint " + ref + "]\n" + strings.Repeat("Derived evidence with uncertainty. ", 200)},
		{Role: "user", Content: "[Memory checkpoint " + ref + "]\n" + strings.Repeat("Earlier derived evidence. ", 200)},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "pending", Name: "write", Arguments: `{"path":"output.txt"}`}}},
		{Role: "user", Content: "[Recorded answer for this request]\nPermission remains required."},
		{Role: "user", Content: "Latest correction"},
	}
	before := mustMarshalMessages(t, messages)
	compacted, err := a.CompactWorkContext(ctx, messages, nil, 2200)
	require.NoError(t, err)
	require.LessOrEqual(t, WorkContextSize(compacted, nil), 2200)
	require.Zero(t, model.calls, "existing derived memories need exact archival, not repeated inference")
	require.Equal(t, messages[:3], compacted[:3])
	require.Equal(t, messages[5:], compacted[len(compacted)-3:], "keep pending call and current human direction exact")
	require.Equal(t, before, mustMarshalMessages(t, messages), "saved continuation remains immutable")
	archive := workContextHash("exchange-archive-v1", mustMarshalMessages(t, messages[3:5]))
	require.Contains(t, compacted[3].Content, archive)
	for _, fingerprint := range []string{archive, ref} {
		result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": fingerprint, "messageIndex": 0, "maxChars": 4000}, 0)
		require.NoError(t, err)
		require.NotContains(t, string(result[0].Payload), `"matches":[]`, "another replica can recover both archive layers")
	}
	run, _ := common.RunFromContext(ctx)
	rows, err := b.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	retained := map[string]string{}
	for _, row := range rows {
		data, _ := row["data"].(map[string]any)
		fingerprint, _ := data["contextHash"].(string)
		exact, _ := data["sourceMessages"].(string)
		retained[fingerprint] = exact
	}
	require.Equal(t, string(mustMarshalMessages(t, messages[3:5])), retained[archive])
	require.Equal(t, string(raw), retained[ref], "archiving derived memory must preserve its original source")
	_, err = a.CompactWorkContext(ctx, messages, nil, 1)
	require.Error(t, err, "archival must not expand a hard target or retire required input")
	require.Zero(t, model.calls)

	// Already-small archive envelopes cannot be usefully wrapped again. The
	// storage bound must terminate without creating larger replacement layers.
	small := append([]common.ChatMessage{}, messages[:3]...)
	small = append(small, common.ChatMessage{Role: "user", Content: "[Memory checkpoint small]\nTiny retained note"})
	small = append(small, messages[5:]...)
	priorRows, err := b.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	_, err = a.CompactWorkContext(ctx, small, nil, 1)
	require.Error(t, err)
	currentRows, err := b.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	require.Len(t, currentRows, len(priorRows), "no archive is written without strict size reduction")
	require.Zero(t, model.calls)
}
