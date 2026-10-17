package planner

import (
	"encoding/json"
	"fmt"
)

// Classification needs a bounded recent window, not a model-generated summary.
// The complete captured conversation stays on the goal for the execution loop
// and recallWorkHistory. Long chats must not make the cheap classifier unbounded.
func conversationPreview(conversation any) (string, error) {
	raw, err := json.Marshal(conversation)
	if err != nil {
		return "", err
	}
	if len(raw) <= 20000 {
		return string(raw), nil
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("invalid conversation: %w", err)
	}
	// Keep complete source responses separate from message excerpts. A cut JSON
	// receipt could hide its timestamp, location or error/truncation marker.
	if evidence, ok := value["sourceContext"].(map[string]any); ok {
		preview := map[string]any{"note": evidence["note"], "limitations": evidence["limitations"]}
		sources, _ := evidence["recordedSources"].([]any)
		keptSources := []any{}
		for _, source := range sources {
			candidate := append(append([]any{}, keptSources...), source)
			preview["recordedSources"] = candidate
			encoded, _ := json.Marshal(preview)
			if len(encoded) > 10000 {
				preview["previewNotice"] = "Some sources omitted; omitted evidence is not verified. Full receipts remain in saved conversation input."
				continue
			}
			keptSources = candidate
		}
		preview["recordedSources"] = keptSources
		value["sourceContext"] = preview
	}
	messages, _ := value["messages"].([]any)
	kept := []any{}
	remaining := 16000
	for n := len(messages) - 1; n >= 0 && remaining > 0; n-- {
		message, ok := messages[n].(map[string]any)
		if !ok {
			continue
		}
		content, _ := message["content"].(string)
		runes := []rune(content)
		// Limit an individual giant message as well as the whole window.
		limit := min(1000, remaining)
		if len(runes) > limit {
			content = string(runes[:limit]) + " [excerpt; full message retained]"
		}
		kept = append(kept, map[string]any{"role": message["role"], "content": content})
		remaining -= len([]rune(content))
	}
	for a, b := 0, len(kept)-1; a < b; a, b = a+1, b-1 {
		kept[a], kept[b] = kept[b], kept[a]
	}
	value["messages"] = kept
	if page, ok := value["pageContext"].(string); ok && len([]rune(page)) > 500 {
		value["pageContext"] = string([]rune(page)[:500])
	}
	value["contextNotice"] = "Recent excerpts only. Older messages and full text remain saved. Missing personal facts or unresolved references require retrieval; do not infer that they were never supplied."
	raw, err = json.Marshal(value)
	// Enforce the encoded-byte budget even for Unicode and source-rich chats.
	for err == nil && len(raw) > 20000 && len(kept) > 1 {
		kept = kept[1:]
		value["messages"] = kept
		raw, err = json.Marshal(value)
	}
	return string(raw), err
}
