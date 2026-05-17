package cognition

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// generateUnifiedStreaming drives a bounded multi-turn streaming tool-calling
// loop. Text content streams to the frontend as it arrives; tool call results
// are fed back to the model on subsequent turns so the reply can incorporate
// the retrieved data. See runStreamingToolLoop for loop semantics.
func (c *CognitionIntegration) generateUnifiedStreaming(
	ctx context.Context,
	agent *agentPayload,
	trigger string,
	spaceId string,
	participantId string,
	replyId string,
	participants []map[string]any,
	history []conversationMessage,
	si spaceInfo,
	attachmentSummaries []map[string]any,
) (*streamResult, error) {

	if c == nil || c.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}

	streamProvider := c.engine.ChatStreamWithToolsProviderByName("stream54Mini")
	if streamProvider == nil {
		return nil, fmt.Errorf("stream-with-tools provider not available")
	}

	personality := strings.TrimSpace(agent.Personality)
	if personality == "" {
		personality = "You are a helpful, professional assistant that supports users in their sessions. You respond when asked questions or when you can provide relevant insights."
	}

	spaceContext := c.getSpaceContextForPromptCached(ctx, strings.TrimSpace(spaceId))

	data := map[string]any{
		"trigger": strings.TrimSpace(trigger),
		"assistant": map[string]any{
			"name":        strings.TrimSpace(agent.Name),
			"personality": personality,
		},
		"space":             buildSpaceData(strings.TrimSpace(spaceId), si),
		"participants":      participants,
		"history":           []map[string]any{},
		"historyInMessages": true,
	}
	if handoff := handoffFromContext(ctx); handoff != "" {
		data["handoffFrom"] = handoff
	}
	if len(spaceContext) > 0 {
		data["spaceContext"] = spaceContext
	}
	if len(attachmentSummaries) > 0 {
		data["attachmentSummaries"] = attachmentSummaries
	}

	prompt, err := c.engine.RenderPrompt("cognitionReply", data)
	if err != nil {
		return nil, fmt.Errorf("render prompt: %w", err)
	}

	messages := []common.ChatMessage{
		{Role: "system", Content: prompt},
	}
	for _, msg := range history {
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if role != "" && content != "" {
			messages = append(messages, common.ChatMessage{Role: role, Content: content})
		}
	}

	toolNames := c.toolsForContext(si.spaceType, false, agent)
	tools := c.engine.ToolDefinitionsForNames(toolNames)

	return c.runStreamingToolLoop(ctx, streamProvider, messages, tools, spaceId, participantId, replyId, "unified-stream")
}
