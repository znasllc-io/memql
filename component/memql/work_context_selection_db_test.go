package memql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

// A short earlier exchange precedes the latest pinned correction. It must not
// prevent the larger completed tool exchanges after that correction from
// making room. These are synthetic contents with the failed run's dimensions.
func contextPinnedCorrectionTrace() ([]common.ChatMessage, []common.ToolDefinition) {
	messages := []common.ChatMessage{
		{Role: "system", Content: strings.Repeat("s", 18318)},
		{Role: "user", Content: strings.Repeat("u", 8)},
		{Role: "assistant", Content: strings.Repeat("a", 57)},
		{Role: "user", Content: strings.Repeat("u", 216)},
		{Role: "user", Content: strings.Repeat("c", 113)},
	}
	for index, size := range []int{6868, 3762, 3289, 8810} {
		call := fmt.Sprintf("read-%d", index)
		messages = append(messages,
			common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: call, Name: "read", Arguments: `{}`}}},
			common.ChatMessage{Role: "tool", ToolCallId: call, Content: strings.Repeat(fmt.Sprint(index), size)},
		)
	}
	tools := []common.ToolDefinition{{Name: "read", InputSchema: map[string]any{"description": strings.Repeat("contract ", 2500)}}}
	return messages, tools
}

func TestWorkContextCompactsPastSmallPrefixAndPinnedCorrectionAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	reply, err := json.Marshal(workCheckpoint{Facts: []string{strings.Repeat("Evidence ", 43)}, Entities: []string{}, Decisions: []string{}, Constraints: []string{}, Unfinished: []string{}})
	require.NoError(t, err)
	model := &checkpointModel{reply: string(reply)}
	contextTestModel(a, model)
	messages, tools := contextPinnedCorrectionTrace()
	before, err := json.Marshal(messages)
	require.NoError(t, err)
	target := WorkContextTarget(messages, tools, 20000)
	require.Greater(t, WorkContextSize(messages, tools), target)
	compacted, err := a.CompactWorkContext(ctx, messages, tools, target)
	require.NoError(t, err)
	require.LessOrEqual(t, WorkContextSize(compacted, tools), target)
	require.Equal(t, messages[:5], compacted[:5], "keep the small exchange and both pinned requests exact")
	require.Equal(t, messages[len(messages)-2:], compacted[len(compacted)-2:], "keep the latest call and result intact")
	require.Positive(t, model.calls)
	require.LessOrEqual(t, model.calls, 8, "selection must retain the existing summarizer ceiling")
	for _, unit := range workContextUnits(compacted) {
		require.True(t, unit.complete)
	}
	after, err := json.Marshal(messages)
	require.NoError(t, err)
	require.Equal(t, before, after, "do not rewrite the saved continuation")

	// A different replica must reuse the durable checkpoints and recover exact
	// tool evidence without access to the first replica's model or memory.
	peerModel := &checkpointModel{fail: true}
	contextTestModel(b, peerModel)
	again, err := b.CompactWorkContext(ctx, messages, tools, target)
	require.NoError(t, err)
	require.Equal(t, compacted, again)
	require.Zero(t, peerModel.calls)
	fingerprint := workCheckpointHash(mustMarshalMessages(t, messages[5:7]), workContextTask(messages))
	var recovered strings.Builder
	for offset := 0; ; {
		result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": fingerprint, "messageIndex": 1, "offset": offset, "maxChars": 4000}, 0)
		require.NoError(t, err)
		var recalled struct {
			Matches []struct {
				Source     string
				NextOffset int
			}
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &recalled))
		require.Len(t, recalled.Matches, 1)
		page := recalled.Matches[0]
		recovered.WriteString(page.Source)
		if page.NextOffset == 0 {
			break
		}
		require.Greater(t, page.NextOffset, offset)
		offset = page.NextOffset
	}
	exact, err := json.Marshal(messages[6])
	require.NoError(t, err)
	require.Equal(t, string(exact), recovered.String())
}

func TestWorkContextUnproductiveCandidatesKeepTheModelCallBound(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, e)
	model := &checkpointModel{}
	contextTestModel(e, model)
	messages := []common.ChatMessage{{Role: "system", Content: "Follow the owner"}, {Role: "user", Content: "Original request"}}
	for index := range 12 {
		messages = append(messages,
			common.ChatMessage{Role: "assistant", Content: fmt.Sprintf("Small historical note %d", index)},
			common.ChatMessage{Role: "developer", Content: "Pinned policy"},
		)
	}
	messages = append(messages, common.ChatMessage{Role: "user", Content: "Current request"}, common.ChatMessage{Role: "assistant", Content: "Active exchange"})
	before := mustMarshalMessages(t, messages)
	_, err := e.CompactWorkContext(ctx, messages, nil, 1)
	require.ErrorContains(t, err, "context checkpoint budget exhausted; originals were retained")
	require.Equal(t, 8, model.calls, "trying another candidate must not add an unbounded model loop")
	require.Equal(t, before, mustMarshalMessages(t, messages))
}

func TestWorkContextArchivesRemainingResultsAfterBoundedSummaries(t *testing.T) {
	for _, count := range []int{60, 80} {
		t.Run(fmt.Sprintf("%d-completed-reads", count), func(t *testing.T) {
			a, _, _ := readMergeTestEngine(t)
			b, _, _ := readMergeTestEngine(t)
			ctx := contextTestRun(t, a)
			model := &checkpointModel{}
			contextTestModel(a, model)
			messages := []common.ChatMessage{{Role: "system", Content: "Keep the original evidence"}, {Role: "user", Content: "Continue the same authorized work"}}
			for n := range count {
				call := fmt.Sprintf("read-%02d", n)
				messages = append(messages,
					common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: call, Name: "read", Arguments: `{}`}}},
					common.ChatMessage{Role: "tool", Name: "read", ToolCallId: call, Content: strings.Repeat(call+" evidence ", 350)})
			}
			messages = append(messages, common.ChatMessage{Role: "assistant", Content: "The completed reads are recorded."})
			before := mustMarshalMessages(t, messages)
			compacted, err := a.CompactWorkContext(ctx, messages, nil, 20000)
			require.NoError(t, err, "medium completed results can be archived when the bounded semantic pass is exhausted")
			require.Equal(t, 8, model.calls)
			require.LessOrEqual(t, WorkContextSize(compacted, nil), 20000)
			require.Equal(t, before, mustMarshalMessages(t, messages))
			require.Equal(t, messages[:2], compacted[:2])
			for _, unit := range workContextUnits(compacted) {
				require.True(t, unit.complete)
			}
			// Every byte remains available to another replica, including the original
			// sources of summaries and the results archived by the final fallback.
			exact := map[string]bool{}
			remember := func(entries []common.ChatMessage) {
				for _, entry := range entries {
					raw, err := json.Marshal(entry)
					require.NoError(t, err)
					exact[string(raw)] = true
				}
			}
			remember(compacted)
			run, _ := common.RunFromContext(ctx)
			rows, err := b.workRows(ContextWithFreshRead(ctx), "workObservationsForOwnerRun", run.RunId)
			require.NoError(t, err)
			for _, row := range rows {
				data, _ := row["data"].(map[string]any)
				source, _ := data["sourceMessages"].(string)
				if source == "" {
					continue
				}
				var archived []common.ChatMessage
				require.NoError(t, json.Unmarshal([]byte(source), &archived))
				remember(archived)
			}
			for _, message := range messages {
				raw, _ := json.Marshal(message)
				require.True(t, exact[string(raw)], "exact source missing: %s", message.ToolCallId)
			}
			peerModel := &checkpointModel{fail: true}
			contextTestModel(b, peerModel)
			again, err := b.CompactWorkContext(ctx, messages, nil, 20000)
			require.NoError(t, err)
			require.Equal(t, compacted, again)
			require.Zero(t, peerModel.calls, "recovery reuses valid summaries instead of repeating research")
		})
	}
}

func mustMarshalMessages(t *testing.T, messages []common.ChatMessage) []byte {
	t.Helper()
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	return raw
}
