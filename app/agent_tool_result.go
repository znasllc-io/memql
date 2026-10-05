package app

import (
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// The engine's tool surface is MCP-compatible. The local model already gets a
// tool message, so nesting the text inside another escaped content envelope
// wastes context and obscures receipts. Keep non-text/multiple-block results
// intact; unwrap exactly the engine's single text block, never arbitrary data.
func agentToolResultText(raw string) (string, error) {
	var result memql.ToolCallResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", fmt.Errorf("decode engine tool result: %w", err)
	}
	if result.IsError {
		if len(result.Content) == 1 && result.Content[0].Type == "text" {
			return "", fmt.Errorf("%s", result.Content[0].Text)
		}
		return "", fmt.Errorf("engine tool refused: %s", raw)
	}
	if len(result.Content) == 1 && result.Content[0].Type == "text" {
		return result.Content[0].Text, nil
	}
	return raw, nil
}
