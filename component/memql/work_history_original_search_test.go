package memql

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkHistoryOriginalSearchPreservesOffsetsAndExcludesPrivateArchiveFields(t *testing.T) {
	needle := "line one\n\"文献🧪\" C:\\source"
	call := common.ToolCall{ID: "opaque-287", Name: "readInventory", Arguments: `{"path":"collection.csv"}`}
	message := common.ChatMessage{Role: "tool", Name: call.Name, ToolCallId: call.ID, Content: historySearchEnvelope(t, strings.Repeat("İ文🧪 ", 200)+needle+strings.Repeat(" tail", 100))}
	raw, _ := json.Marshal(message)
	page, ok := workHistoryOriginalPage(message, call, needle, 0, 400, false)
	require.True(t, ok)
	position := workHistorySearchPosition(string(raw), strings.ToLower(needle))
	wantOffset := utf8.RuneCount(raw[:position]) - 120
	require.Equal(t, wantOffset, page["offset"])
	require.Equal(t, string([]rune(string(raw))[wantOffset:wantOffset+400]), page["source"])
	_, ok = workHistoryOriginalPage(message, call, "287", 0, 400, false)
	require.False(t, ok, "generated call metadata is not source text")
	page, ok = workHistoryOriginalPage(message, call, "collection.csv", 0, 400, false)
	require.True(t, ok)
	require.Equal(t, "arguments", page["matchedField"])
	require.Equal(t, string([]rune(string(raw))[:400]), page["source"])
	private := common.ChatMessage{Role: "assistant", Content: "PRIVATE-REASONING", ToolCalls: []common.ToolCall{call, {ID: "secret", Name: "readSecret", Arguments: `{"value":"PRIVATE-SECRET"}`}, {ID: "recall", Name: "recallWorkHistory", Arguments: `{"search":"RECALL-ECHO"}`}}}
	_, ok = workHistoryOriginalPage(private, common.ToolCall{}, "collection.csv", 0, 400, false)
	require.False(t, ok, "argument searches return completed results, never raw assistant prose")
	directory := workHistoryOriginalDirectory([]common.ChatMessage{private, message, {Role: "tool", Name: "readSecret", ToolCallId: "secret"}, {Role: "tool", Name: "recallWorkHistory", ToolCallId: "recall"}}, map[int]common.ToolCall{1: call})
	encoded, _ := json.Marshal(workHistorySourceIndex("archive", directory, 0))
	require.Contains(t, string(encoded), "collection.csv")
	for _, forbidden := range []string{"PRIVATE-REASONING", "PRIVATE-SECRET", "RECALL-ECHO", "readSecret", "recallWorkHistory"} {
		require.NotContains(t, string(encoded), forbidden)
	}
	require.Equal(t, 4, len(directory), "filtering must not renumber old archive indexes")
}

func TestWorkHistoryDefaultSearchSkipsRecallEchoesAndOpaqueIDsAcrossReplicas(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	run, _ := common.RunFromContext(ctx)
	access, _ := auth.AccessFromContext(ctx)
	sequence := 0
	started := time.Now().UTC().Add(-time.Hour)
	insert := func(messages []common.ChatMessage, archive bool) string {
		sequence++
		createdAt := started.Add(time.Duration(sequence) * time.Millisecond)
		raw, err := json.Marshal(messages)
		require.NoError(t, err)
		data := map[string]any{"delegatedEvidence": true, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(raw)}
		ref := uniqueSuffix(fmt.Sprintf("original-search-%d", sequence))
		observationID := ref
		if archive {
			ref = workContextHash("tool-v2", raw)
			observationID = "checkpoint-" + ref + "-" + BareShortId(run.RunId)
			data = map[string]any{"sourceMessages": string(raw), "contextHash": ref}
		}
		payload, err := json.Marshal(map[string]any{"runId": "v1:work:run:" + BareShortId(run.RunId), "ownerUserId": access.UserId, "kind": "note", "createdAt": createdAt.Format(time.RFC3339Nano), "data": data})
		require.NoError(t, err)
		_, err = db.DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES($1,'v1:work:observation',$2,'original-search-test','{}',$3)`, "v1:work:observation:"+observationID, createdAt, string(payload))
		require.NoError(t, err)
		return ref
	}
	needle := "287 museum specimens 🧪"
	original := common.ChatMessage{Role: "tool", Name: "readInventory", ToolCallId: "actual-source", Content: strings.Repeat("文献 metadata ", 400) + needle + "\nverified original"}
	originalRef := insert([]common.ChatMessage{
		{Role: "system", Content: "PRIVATE-SYSTEM"},
		{Role: "assistant", Content: "PRIVATE-REASONING", ToolCalls: []common.ToolCall{{ID: original.ToolCallId, Name: original.Name, Arguments: `{"path":"collection.csv"}`}}}, original,
	}, false)
	legacy := common.ChatMessage{Role: "tool", Name: "readInventory", ToolCallId: "legacy-source", Content: "ARCHIVE-ONLY specimen record"}
	legacyRef := insert([]common.ChatMessage{legacy}, true)
	for n := range 40 {
		name, args, content := "recallWorkHistory", `{"search":"287"}`, "Repeated recall echo: "+needle
		if n%3 == 1 {
			name, args = "executeCapability", `{"name":"work.workSourceContext","arguments":{}}`
		} else if n%3 == 2 {
			name, args, content = "readInventory", `{}`, "No matching specimens"
		}
		id := fmt.Sprintf("opaque-287-%d", n)
		insert([]common.ChatMessage{{Role: "user", Content: "[Memory checkpoint prior]\nDerived echo: " + needle}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: id, Name: name, Arguments: args}}}, {Role: "tool", Name: name, ToolCallId: id, Content: content}}, n%2 == 0)
	}
	var recoveredRef string
	pages := 0
	for cursor := ""; ; {
		result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"search": "287", "cursor": cursor, "maxChars": 400}, 0)
		require.NoError(t, err)
		var page struct {
			Matches []struct {
				Checkpoint, Source, SourceRunID string
				Index                           int
				SourceEvidence                  struct{ SHA256 string }
			}
			Cursor string
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &page))
		for _, match := range page.Matches {
			require.Equal(t, originalRef, match.Checkpoint, "a recalled claim or generated call ID is not original evidence")
			require.Contains(t, match.Source, needle)
			require.Equal(t, BareShortId(run.RunId), match.SourceRunID)
			require.Equal(t, 1, match.Index)
			raw, _ := json.Marshal(original)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), match.SourceEvidence.SHA256)
			recoveredRef = match.Checkpoint
		}
		pages++
		require.Less(t, pages, 5)
		if page.Cursor == "" {
			break
		}
		require.NotEqual(t, cursor, page.Cursor)
		cursor = page.Cursor
	}
	require.Equal(t, 2, pages, "bounded scanning continues beyond 32 receipts")
	require.Equal(t, originalRef, recoveredRef)
	// Sources retained only by compaction still participate in the default
	// search. Their original checkpoint/index route remains unchanged.
	result, err := b.recallWorkHistoryBuiltin(ctx, map[string]any{"search": "ARCHIVE-ONLY"}, 0)
	require.NoError(t, err)
	for cursor := ""; !strings.Contains(string(result[0].Payload), "ARCHIVE-ONLY"); {
		var page struct{ Cursor string }
		require.NoError(t, json.Unmarshal(result[0].Payload, &page))
		require.NotEmpty(t, page.Cursor)
		require.NotEqual(t, cursor, page.Cursor)
		cursor = page.Cursor
		result, err = b.recallWorkHistoryBuiltin(ctx, map[string]any{"search": "ARCHIVE-ONLY", "cursor": cursor}, 0)
		require.NoError(t, err)
	}
	require.Contains(t, string(result[0].Payload), legacyRef)
	result, err = b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": legacyRef, "messageIndex": 0}, 0)
	require.NoError(t, err)
	require.Contains(t, string(result[0].Payload), "ARCHIVE-ONLY")
	privateArchive := []common.ChatMessage{
		{Role: "assistant", Content: "PRIVATE-REASONING", ToolCalls: []common.ToolCall{{ID: "public", Name: "readInventory", Arguments: `{"path":"collection-private.csv"}`}, {ID: "secret", Name: "readSecret", Arguments: `{"value":"PRIVATE-SECRET"}`}, {ID: "echo", Name: "recallWorkHistory", Arguments: `{"search":"RECALL-ECHO"}`}}},
		{Role: "tool", Name: "readInventory", ToolCallId: "public", Content: "Public inventory result"},
		{Role: "tool", Name: "readSecret", ToolCallId: "secret", Content: "PRIVATE-SECRET"},
		{Role: "tool", Name: "recallWorkHistory", ToolCallId: "echo", Content: "RECALL-ECHO"},
	}
	privateRef := insert(privateArchive, true)
	result, err = b.recallWorkHistoryBuiltin(ctx, map[string]any{"search": "collection-private.csv"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(result[0].Payload), "Public inventory result")
	require.Contains(t, string(result[0].Payload), `"matchedField":"arguments"`)
	for _, forbidden := range []string{"PRIVATE-REASONING", "PRIVATE-SECRET", "RECALL-ECHO", "readSecret"} {
		require.NotContains(t, string(result[0].Payload), forbidden)
	}
	result, err = b.recallWorkHistoryBuiltin(ctx, map[string]any{"checkpoint": privateRef, "messageIndex": 0}, 0)
	require.NoError(t, err)
	require.Contains(t, string(result[0].Payload), "PRIVATE-REASONING", "an explicit owner-authorized checkpoint read remains exact")
	_, err = b.recallWorkHistoryBuiltin(common.ContextWithRun(askTestActor(), run), map[string]any{"search": "287"}, 0)
	require.Error(t, err)
}
