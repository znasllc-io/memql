package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkHistoryIndexLocatesOriginalEvidenceAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	messages := []common.ChatMessage{
		{Role: "user", Content: "[Memory checkpoint older]\nDerived summary"},
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "read", Name: "readPaper", Arguments: `{"url":"https://example.test/paper"}`}}},
		{Role: "tool", Name: "readPaper", ToolCallId: "read", Content: "Measured evidence from the original paper"},
	}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	hash := workContextHash("summary-v2", raw)
	require.NoError(t, a.saveWorkContextSource(ctx, hash, string(raw), ""))
	result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": hash, "messageIndex": 0}, 0)
	require.NoError(t, err)
	var response struct {
		Sources []struct {
			Messages []struct {
				Index int
				Role  string
				Kind  string
				Tool  string
			}
		}
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Len(t, response.Sources, 1)
	index := response.Sources[0].Messages
	require.Len(t, index, 3)
	require.Equal(t, "derived-checkpoint", index[0].Kind)
	require.Equal(t, "tool", index[2].Role)
	require.Equal(t, "readPaper", index[2].Tool)
	page, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": hash, "messageIndex": index[2].Index}, 0)
	require.NoError(t, err)
	require.Contains(t, string(page[0].Payload), "Measured evidence from the original paper")
	// The directory is descriptive metadata; stored source bytes stay exact.
	call, _ := parser.RenderCall("workCheckpointForOwner", map[string]any{"fingerprint": hash})
	saved, err := a.Execute(ContextWithFreshRead(ctx), "query "+call)
	require.NoError(t, err)
	require.Equal(t, string(raw), MaterializeRows(saved.OutputPayload())[0]["data"].(map[string]any)["sourceMessages"])
}

func TestWorkHistoryIndexBoundsMetadataAndAlwaysAdvances(t *testing.T) {
	messages := make([]common.ChatMessage, 30)
	for index := range messages {
		messages[index] = common.ChatMessage{Role: "assistant", Name: strings.Repeat("\u2028", 1000), ToolCalls: []common.ToolCall{
			{Name: strings.Repeat("\u2028", 1000), Arguments: strings.Repeat("\u2028", 1000)},
			{Name: strings.Repeat("\u2028", 1000), Arguments: strings.Repeat("\u2028", 1000)},
		}}
	}
	seen := 0
	for start := 0; ; {
		page := workHistorySourceIndex("checkpoint", messages, start)
		raw, err := json.Marshal(page)
		require.NoError(t, err)
		require.Less(t, len(raw), 2700)
		entries := page["messages"].([]any)
		require.NotEmpty(t, entries)
		for _, entry := range entries {
			require.Equal(t, seen, entry.(map[string]any)["index"])
			seen++
		}
		next, more := page["nextMessageIndex"].(int)
		if !more {
			break
		}
		require.Greater(t, next, start)
		start = next
	}
	require.Equal(t, len(messages), seen)
}

func TestWorkHistorySearchJumpsWithinAnExactCheckpointAcrossReplicas(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	messages := []common.ChatMessage{{Role: "tool", Name: "readPaper", Content: strings.Repeat("metadata 文献 ", 1000) + "ABSTRACT: a verified experimental result"}}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	hash := workContextHash("tool-v2", raw)
	require.NoError(t, a.saveWorkContextSource(ctx, hash, string(raw), ""))
	args := map[string]any{"checkpoint": hash, "messageIndex": 0, "search": "abstract", "maxChars": 400}
	result, err := b.recallWorkHistoryBuiltin(ctx, args, 0)
	require.NoError(t, err)
	var response struct {
		Matches []struct {
			Source string
			Offset int
		}
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Len(t, response.Matches, 1)
	require.Greater(t, response.Matches[0].Offset, 10000)
	require.Contains(t, response.Matches[0].Source, "ABSTRACT: a verified experimental result")
	// Explicit offsets remain exact, even when a search filters the message.
	args["offset"] = 0
	result, err = b.recallWorkHistoryBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Zero(t, response.Matches[0].Offset)
	require.NotContains(t, response.Matches[0].Source, "ABSTRACT:")
	args["search"] = "unavailable finding"
	result, err = b.recallWorkHistoryBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Empty(t, response.Matches)
}

func TestWorkHistoryDecodedSearchAcrossReplicasPreservesArchive(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	needle := "first line\n\"quoted\" C:\\source\\file 🧪"
	messages := []common.ChatMessage{{Role: "tool", Name: "readSource", Content: historySearchEnvelope(t, strings.Repeat("İ文 ", 2000)+needle+strings.Repeat(" tail", 100))}}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)
	hash := workContextHash("tool-v2", raw)
	// Replica B reads before A writes, then must see the peer's fresh archive.
	_, err = b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": hash, "messageIndex": 0}, 0)
	require.NoError(t, err)
	require.NoError(t, a.saveWorkContextSource(ctx, hash, string(raw), ""))
	args := map[string]any{"checkpoint": hash, "messageIndex": 0, "search": needle, "maxChars": 400}
	result, err := b.recallWorkHistoryBuiltin(ctx, args, 0)
	require.NoError(t, err)
	var response struct {
		Matches []struct {
			Source     string
			Offset     int
			NextOffset int
		}
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Len(t, response.Matches, 1)
	require.Greater(t, response.Matches[0].Offset, 5000)
	want, err := json.Marshal(messages[0])
	require.NoError(t, err)
	page := response.Matches[0]
	require.Equal(t, string([]rune(string(want))[page.Offset:page.NextOffset]), page.Source)
	// Searching changes only the starting page. Explicit pages still reconstruct
	// the original serialized message, including every escape and metadata key.
	var exact strings.Builder
	args["maxChars"] = 4000
	for offset := 0; ; {
		args["offset"] = offset
		result, err = b.recallWorkHistoryBuiltin(ctx, args, 0)
		require.NoError(t, err)
		response.Matches = nil
		require.NoError(t, json.Unmarshal(result[0].Payload, &response))
		require.Len(t, response.Matches, 1)
		page = response.Matches[0]
		require.Equal(t, offset, page.Offset)
		exact.WriteString(page.Source)
		if page.NextOffset == 0 {
			break
		}
		require.Greater(t, page.NextOffset, offset)
		offset = page.NextOffset
	}
	require.Equal(t, string(want), exact.String())
	call, err := parser.RenderCall("workCheckpointForOwner", map[string]any{"fingerprint": hash})
	require.NoError(t, err)
	saved, err := b.Execute(ContextWithFreshRead(ctx), "query "+call)
	require.NoError(t, err)
	require.Equal(t, string(raw), MaterializeRows(saved.OutputPayload())[0]["data"].(map[string]any)["sourceMessages"])
	args["search"] = `first line\n"quoted"`
	result, err = b.recallWorkHistoryBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(result[0].Payload, &response))
	require.Empty(t, response.Matches, "literal escape spelling must not match decoded newline")
}
