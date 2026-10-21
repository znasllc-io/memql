package memql

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// Compaction changes the active conversation, not the source that was read.
// Restore only a paired tool's exact archive under this owner and run. No model
// call or external read is needed, and the existing total evidence budget holds.
func (e *MemQLEngine) workCheckpointSources(ctx context.Context, checkpoints []map[string]any, runID string, remaining *int) ([]map[string]any, bool) {
	cache := map[string]*common.ChatMessage{}
	restore := func(stub common.ChatMessage) (common.ChatMessage, bool) {
		fingerprint := workSourceArchiveFingerprint(stub.Content)
		if fingerprint == "" {
			return common.ChatMessage{}, false
		}
		if _, found := cache[fingerprint]; !found && len(cache) < 8 {
			cache[fingerprint] = nil
			call, err := parser.RenderCall("workContextSourcesForOwnerRun", map[string]any{"runId": runID, "fingerprint": fingerprint})
			if err == nil {
				result, readErr := e.Execute(ContextWithFreshRead(ctx), "query "+call)
				if readErr == nil {
					rows := MaterializeRows(result.OutputPayload())
					if len(rows) == 1 && BareShortId(stringArg(rows[0], "runId")) == BareShortId(runID) {
						data, _ := rows[0]["data"].(map[string]any)
						if message, ok := workSourceArchiveMessage(stringArg(data, "sourceMessages"), fingerprint); ok {
							cache[fingerprint] = &message
						}
					}
				}
			}
		}
		message := cache[fingerprint]
		if message == nil || message.Name != stub.Name || message.ToolCallId != stub.ToolCallId {
			return common.ChatMessage{}, false
		}
		return *message, true
	}
	return projectWorkCheckpointSources(checkpoints, runID, remaining, restore)
}

func workSourceArchiveFingerprint(content string) string {
	header, _, found := strings.Cut(content, "]\n")
	if !found || !strings.HasPrefix(header, "[Archived tool result ") {
		return ""
	}
	fingerprint := strings.TrimPrefix(header, "[Archived tool result ")
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != 32 {
		return ""
	}
	return fingerprint
}

func workSourceArchiveMessage(source, fingerprint string) (common.ChatMessage, bool) {
	if len(source) > 2*1024*1024 || workContextHash("tool-v2", []byte(source)) != fingerprint {
		return common.ChatMessage{}, false
	}
	var messages []common.ChatMessage
	if json.Unmarshal([]byte(source), &messages) != nil || len(messages) != 1 {
		return common.ChatMessage{}, false
	}
	message := messages[0]
	if message.Role != "tool" || message.Name == "" || message.ToolCallId == "" || message.Content == common.UnexecutedWorkToolResult || strings.HasPrefix(message.Content, "[Archived tool result ") {
		return common.ChatMessage{}, false
	}
	return message, true
}
