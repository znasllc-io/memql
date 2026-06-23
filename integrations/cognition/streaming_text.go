package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/core/common"
)

// generateTextStreaming renders the score engineReply.v1 prompt and streams the
// response via CallChatStream, emitting text chunks to the frontend in
// near-real-time. Returns the fully assembled response text.
//
// replyId is the stable per-turn id stamped on every emitted chunk. The same
// id becomes the committed utterance.id at the end of the turn so the
// frontend can key its rendered bubble by it. Callers generate replyId once
// per turn upstream (see cognition_handler.go) and thread it through.
func (c *CognitionIntegration) generateTextStreaming(
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
) (string, error) {

	if c == nil || c.engine == nil {
		return "", fmt.Errorf("engine not configured")
	}

	streamProvider := c.engine.ChatStreamProviderByName("stream54Mini")
	if streamProvider == nil {
		streamProvider = c.engine.ChatStreamProvider()
	}
	if streamProvider == nil {
		return "", fmt.Errorf("stream provider not available")
	}

	personality := strings.TrimSpace(agent.Personality)
	if personality == "" {
		personality = "You are a helpful, professional assistant that supports users in their sessions. You respond when asked questions or when you can provide relevant insights."
	}

	spaceContext := c.getSpaceContextForPromptCached(ctx, strings.TrimSpace(partitionId))

	historyForPrompt := make([]map[string]any, 0, len(history))
	for _, msg := range history {
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if role == "" && content == "" {
			continue
		}
		historyForPrompt = append(historyForPrompt, map[string]any{
			"role":    role,
			"content": content,
		})
	}

	data := map[string]any{
		"trigger": strings.TrimSpace(trigger),
		"assistant": map[string]any{
			"name":        strings.TrimSpace(agent.Name),
			"personality": personality,
		},
		"space":        buildSpaceData(strings.TrimSpace(partitionId), si),
		"participants": participants,
		"history":      historyForPrompt,
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
		return "", fmt.Errorf("render text prompt: %w", err)
	}

	messages := []common.ChatMessage{
		{Role: "system", Content: prompt},
	}
	// Append conversation history as individual messages for better context.
	for _, msg := range history {
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if role != "" && content != "" {
			messages = append(messages, common.ChatMessage{Role: role, Content: content})
		}
	}

	chunks, err := streamProvider.CallChatStream(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("start text stream: %w", err)
	}

	start := time.Now()
	var fullText strings.Builder
	var chunkBuffer strings.Builder
	var chunkIndex int

	for chunk := range chunks {
		if chunk.Error != nil {
			c.Logger.Warn("streaming-text: stream error", "error", chunk.Error)
			break
		}
		if chunk.Done {
			break
		}

		fullText.WriteString(chunk.Content)
		chunkBuffer.WriteString(chunk.Content)

		// Emit when buffer reaches a sentence boundary or ~80 chars.
		if shouldFlushTextBuffer(chunkBuffer.String()) {
			c.mutationEmitTextChunk(ctx, partitionId, participantId, replyId, chunkBuffer.String(), chunkIndex, false)
			chunkBuffer.Reset()
			chunkIndex++
		}
	}

	// Flush remaining buffer.
	remaining := chunkBuffer.String()
	if remaining != "" {
		c.mutationEmitTextChunk(ctx, partitionId, participantId, replyId, remaining, chunkIndex, false)
		chunkIndex++
	}

	assembled := stripRawToolCalls(strings.TrimSpace(fullText.String()))

	// Send final done signal.
	c.mutationEmitTextChunk(ctx, partitionId, participantId, replyId, "", chunkIndex, true)

	c.Logger.Info("streaming-text: complete",
		"partitionId", partitionId,
		"chunks", chunkIndex,
		"totalChars", len(assembled),
		"totalMs", time.Since(start).Milliseconds(),
	)

	return assembled, nil
}

// shouldFlushTextBuffer returns true when the buffer has accumulated enough
// text at a natural boundary (sentence end, newline) or exceeds ~80 chars.
func shouldFlushTextBuffer(buf string) bool {
	if len(buf) < 15 {
		return false
	}
	if len(buf) >= 80 {
		return true
	}
	trimmed := strings.TrimRightFunc(buf, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	last := trimmed[len(trimmed)-1]
	return last == '.' || last == '!' || last == '?' || last == '\n' || last == ':'
}

// mutationEmitTextChunk inserts a v1:cognition:text:chunk node via the
// emitTextChunk mutation.
//
// replyId is the stable per-turn id (same across every chunk in this agent
// reply, and equal to the eventual committed utterance.id). The frontend
// uses it to key the rendered bubble so the streaming-then-committed
// transition is in-place -- no React remount, no avatar flicker. Every
// caller MUST supply a non-empty replyId; passing "" is a bug.
func (c *CognitionIntegration) mutationEmitTextChunk(ctx context.Context, partitionId, participantId, replyId, text string, index int, done bool) {
	// Colon-free chunkId. The dsl mutation's id template is
	// `concat("text-chunk-", args.chunkId)`, so anything we pass becomes
	// the persisted row's shortId after the prefix. Canonical-id
	// validation requires shortIds to be a bare slug / UUID with no
	// colons -- so we MUST NOT splice the full partitionId (which is
	// `v1:cognition:space:...`) into chunkId. (memql#372.)
	//
	// Per-chunk uniqueness comes from the (UnixNano, index) pair --
	// per-turn correlation is already carried via replyId. partitionId
	// previously appeared here only to namespace IDs across spaces,
	// which the canonical concept-id system handles for us.
	chunkId := fmt.Sprintf("%d-%d", time.Now().UnixNano(), index)

	doneStr := "false"
	if done {
		doneStr = "true"
	}

	query := fmt.Sprintf(`mutationEmitTextChunk({
		"chunkId": %s,
		"partitionId": %s,
		"participantId": %s,
		"replyId": %s,
		"text": %s,
		"index": %d,
		"done": %s
	})`,
		escapeJSONString(chunkId),
		escapeJSONString(partitionId),
		escapeJSONString(participantId),
		escapeJSONString(replyId),
		escapeJSONString(text),
		index,
		doneStr,
	)

	if _, err := c.engine.Execute(ctx, query); err != nil {
		c.Logger.Warn("text-chunk: failed to execute", "error", err, "partitionId", partitionId, "replyId", replyId, "index", index)
	}
}
