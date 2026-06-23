package cognition

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// streamResult holds the output of a streaming call.
type streamResult struct {
	Text       string
	TextChunks int
	ToolCalls  []common.ToolCall
}

// generateStreaming drives a bounded multi-turn streaming tool-calling loop.
//
// Text content streams to the frontend as it arrives. Tool calls requested by
// the model are executed and their results are fed back on the next turn so
// lookup tools (e.g. searchUsers) can actually influence the reply. The loop
// is capped at a small number of iterations to bound latency.
//
// replyId is the stable per-turn id stamped on every emitted text-chunk and
// reused as the committed utterance.id so the frontend can key its rendered
// bubble by it across the streaming-then-committed transition.
func (c *CognitionIntegration) generateStreaming(
	ctx context.Context,
	agent *agentPayload,
	trigger string,
	partitionId string,
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

	// Build prompt data.
	personality := strings.TrimSpace(agent.Personality)
	if personality == "" {
		personality = "You are a helpful, professional assistant that supports users in their sessions. You respond when asked questions or when you can provide relevant insights."
	}

	spaceContext := c.getSpaceContextForPromptCached(ctx, strings.TrimSpace(partitionId))

	// Build agent identity for prompt injection.
	assistantData := map[string]any{
		"name":        strings.TrimSpace(agent.Name),
		"personality": personality,
	}
	if desc := strings.TrimSpace(agent.Description); desc != "" {
		assistantData["description"] = desc
	}
	if agent.Capabilities != nil {
		if domains, ok := agent.Capabilities["domains"].([]any); ok && len(domains) > 0 {
			assistantData["domains"] = domains
		}
	}
	if agent.TriggerBehavior != nil {
		if sw, ok := agent.TriggerBehavior["speakWhen"].(string); ok && sw != "" {
			assistantData["speakWhen"] = sw
		}
	}
	toolNames := c.toolsForContext(si.spaceType, false, agent)
	if len(toolNames) > 0 {
		assistantData["tools"] = toolNames
	}
	if agent.ClawCapable() {
		assistantData["workspace"] = "/workspaces/" + strings.TrimSpace(agent.Name)
	}

	data := map[string]any{
		"trigger":           strings.TrimSpace(trigger),
		"assistant":         assistantData,
		"space":             buildSpaceData(strings.TrimSpace(partitionId), si),
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

	tools := c.engine.ToolDefinitionsForNames(toolNames)

	return c.runStreamingToolLoop(ctx, streamProvider, messages, tools, partitionId, participantId, replyId, "stream")
}
