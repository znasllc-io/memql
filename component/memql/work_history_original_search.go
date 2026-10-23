package memql

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/core/common"
)

// Only original tool data participates in discovery. Metadata IDs, assistant
// prose, approval requests and recursive history wrappers can contain the same
// words without being the evidence sought. Explicit checkpoint reads still
// expose their exact archived message under the existing owner checks.
func workHistoryOriginalOperation(call common.ToolCall) bool {
	name := call.Name
	if name == "executeCapability" {
		var args struct{ Name string }
		if json.Unmarshal([]byte(call.Arguments), &args) != nil {
			return false
		}
		name = args.Name
	}
	if strings.Contains(strings.ToLower(name), "secret") {
		return false
	}
	if last := strings.LastIndexByte(name, '.'); last >= 0 {
		name = name[last+1:]
	}
	switch name {
	case "recallWorkHistory", "recallMemory", "workSourceContext", "ai", "runAgentTurn", "workReviewReply", "respondToUser", "requestUserFeedback", "requestComputerUseScope":
		return false
	}
	return true
}

func workHistoryOriginalResult(message common.ChatMessage, paired common.ToolCall) bool {
	if paired.Name != "" && message.Name != "" && paired.Name != message.Name {
		return false
	}
	if paired.Name == "" {
		paired.Name = message.Name
	}
	return message.Role == "tool" && workHistoryOriginalOperation(paired) && message.Content != common.UnexecutedWorkToolResult && !strings.HasPrefix(message.Content, "[Archived tool result ")
}

// A search directory describes only eligible sources, keeping empty slots so
// archived indexes remain stable. Secret/retrieval argument previews and private
// assistant prose must not leak merely because another message matched.
func workHistoryOriginalDirectory(messages []common.ChatMessage, calls map[int]common.ToolCall) []common.ChatMessage {
	public := make([]common.ChatMessage, len(messages))
	completed := map[string]bool{}
	for i, message := range messages {
		if workHistoryOriginalResult(message, calls[i]) {
			public[i] = common.ChatMessage{Role: "tool", Name: message.Name, ToolCallId: message.ToolCallId}
			completed[message.ToolCallId] = true
		}
	}
	for i, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, call := range message.ToolCalls {
			if completed[call.ID] && workHistoryOriginalOperation(call) {
				public[i].Role = "assistant"
				public[i].ToolCalls = append(public[i].ToolCalls, call)
			}
		}
	}
	return public
}

func workHistoryOriginalPage(message common.ChatMessage, paired common.ToolCall, search string, offset, limit int, explicitOffset bool) (map[string]any, bool) {
	if search == "" {
		return workHistoryPage(message, "", offset, limit, explicitOffset)
	}
	if !workHistoryOriginalResult(message, paired) {
		return nil, false
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, false
	}
	text := string(raw)
	match := -1
	fieldMatch := func(key, value string) int {
		encoded, _ := json.Marshal(value)
		// Search the value as a JSON string, then map the hit into the exact
		// original serialization. Never normalize bytes or search field names.
		remaining := workHistorySearchBytes - len(text)
		position := workHistorySearchJSON(string(encoded), strings.ToLower(search), 1, &remaining)
		if position < 0 {
			return -1
		}
		prefix := `"` + key + `":`
		start := strings.Index(text, prefix+string(encoded))
		if start < 0 {
			return -1
		}
		return start + len(prefix) + position
	}
	match = fieldMatch("Content", message.Content)
	if match < 0 && paired.Arguments != "" {
		encoded, _ := json.Marshal(paired.Arguments)
		if workHistorySearchPosition(string(encoded), search) >= 0 {
			// Argument-only hits select their completed result, not an
			// assistant message that may also carry private prose. The
			// directory retains the call index for an explicit argument read.
			page, ok := workHistoryPage(message, "", offset, limit, true)
			page["matchedField"] = "arguments"
			return page, ok
		}
	}
	if match < 0 {
		return nil, false
	}
	if !explicitOffset {
		offset = max(0, utf8.RuneCountInString(text[:match])-120)
	}
	return workHistoryPage(message, "", offset, limit, true)
}
