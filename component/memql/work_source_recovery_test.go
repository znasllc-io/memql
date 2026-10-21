package memql

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func recoveryTestCheckpoint(t *testing.T, callID string, page map[string]any, mcp bool) map[string]any {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"sourceRunId": page["sourceRunId"], "checkpoint": page["checkpoint"], "messageIndex": 1})
	payload := map[string]any{"matches": []any{page}}
	var content []byte
	if mcp {
		value, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"history": map[string]any{"id": "history", "payload": payload}}}})
		content, _ = json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(value)}}})
	} else {
		content, _ = json.Marshal(map[string]any{"nodes": []any{map[string]any{"id": "history", "payload": payload}}})
	}
	messages, err := json.Marshal([]common.ChatMessage{
		{Role: "system", Content: "PRIVATE-SYSTEM"},
		{Role: "assistant", Content: "PRIVATE-REASONING", ToolCalls: []common.ToolCall{{ID: callID, Name: "recallWorkHistory", Arguments: string(args)}}},
		{Role: "tool", Name: "recallWorkHistory", ToolCallId: callID, Content: string(content)},
	})
	require.NoError(t, err)
	return map[string]any{"id": callID, "data": map[string]any{"messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(messages)}}
}

func TestRecoveredSourcesRequireExactCompletePagesAndStayBounded(t *testing.T) {
	message := common.ChatMessage{Role: "tool", Name: "workbenchHost", ToolCallId: "original", Content: strings.Repeat("文献 original source\n", 450)}
	raw, _ := json.Marshal(message)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	makePage := func(offset int) map[string]any {
		page, ok := workHistoryPage(message, "", offset, 4000, true)
		require.True(t, ok)
		page["sourceRunId"], page["checkpoint"], page["index"] = "prior", "original", 1
		page["sourceEvidence"] = map[string]any{"sha256": digest, "operation": "workbenchHost", "path": "inputs/ledger.csv"}
		return page
	}
	checkpoints := []map[string]any{}
	for offset := 0; ; {
		page := makePage(offset)
		checkpoints = append(checkpoints, recoveryTestCheckpoint(t, fmt.Sprint(offset), page, offset != 0))
		next, ok := page["nextOffset"].(int)
		if !ok {
			break
		}
		offset = next
	}
	for _, tc := range []struct {
		name     string
		rows     []map[string]any
		complete bool
	}{
		{"complete", checkpoints, true},
		{"duplicate", append(append([]map[string]any{}, checkpoints...), checkpoints...), true},
		{"missing first", checkpoints[1:], false},
		{"missing last", checkpoints[:len(checkpoints)-1], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := 2000
			proof, omitted := workRecoveredSources(tc.rows, &budget)
			require.False(t, omitted)
			require.Len(t, proof, 1)
			require.Equal(t, tc.complete, proof[0].Complete)
			require.Equal(t, "inputs/ledger.csv", proof[0].Path)
			encoded, _ := json.Marshal(proof)
			require.NotContains(t, string(encoded), "original source")
			require.NotContains(t, string(encoded), "PRIVATE")
			require.Less(t, len(encoded), 500)
		})
	}
	corrupt := makePage(0)
	corrupt["source"] = "x" + corrupt["source"].(string)[1:]
	rows := append([]map[string]any{recoveryTestCheckpoint(t, "corrupt", corrupt, true)}, checkpoints[1:]...)
	budget := 2000
	proof, _ := workRecoveredSources(rows, &budget)
	require.False(t, proof[0].Complete, "altered bytes cannot retain a complete recovery claim")
	budget = 1
	proof, omitted := workRecoveredSources(checkpoints, &budget)
	require.Empty(t, proof)
	require.True(t, omitted)
	page := makePage(0)
	delete(page, "sourceEvidence")
	budget = 2000
	proof, _ = workRecoveredSources([]map[string]any{recoveryTestCheckpoint(t, "claim", page, true)}, &budget)
	require.Empty(t, proof, "ordinary recalled claims do not have native tool-source provenance")
}
