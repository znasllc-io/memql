package cognition

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// generateVoiceStreaming drives a streaming agent LLM call, accumulates
// tokens into sentences, and dispatches each completed sentence to the
// Bridge Agent for TTS + publish AS the model produces it. Returns the
// full text once the stream completes (so the cognition handler can
// insert it as the committed utterance and the chat UI can render the
// canonical reply alongside the audio).
//
// Compared to the batch generateAIResponse path the voice utterance
// flow used before, time-to-first-audio drops from "wait for full
// agent reply + one TTS call" (~3-4s) to "first sentence boundary +
// streaming-TTS first chunk" (~1-1.5s). The bridge serializes audio
// frames on its per-room sentence queue so playback stays in order
// without cognition needing to wait for each sentence's audio to
// finish before sending the next one.
//
// Sentence segmentation is heuristic, intentionally simple: punctuation
// boundaries (`. `, `? `, `! `, double newline). Edge cases like "Mr.
// Smith" or "U.S." produce slightly stuttery TTS at the false boundary
// and that's fine -- voice cadence sounds natural enough at the cost
// of an occasional micro-pause, and a heavier linguistic split was
// never going to be worth its complexity.
func (c *CognitionIntegration) generateVoiceStreaming(
	ctx context.Context,
	agent *agentPayload,
	trigger string,
	spaceId string,
	winnerAgentName string,
	winnerVoiceModel string,
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
		// Stream provider not wired; caller falls back to batch.
		return "", fmt.Errorf("stream provider not available")
	}

	personality := strings.TrimSpace(agent.Personality)
	if personality == "" {
		personality = "You are a helpful, professional assistant that supports users in their sessions. You respond when asked questions or when you can provide relevant insights."
	}

	spaceContext := c.getSpaceContextForPromptCached(ctx, strings.TrimSpace(spaceId))

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

	data := map[string]any{
		"trigger":           strings.TrimSpace(trigger),
		"assistant":         assistantData,
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
		return "", fmt.Errorf("render prompt: %w", err)
	}

	messages := []common.ChatMessage{{Role: "system", Content: prompt}}
	for _, msg := range history {
		role := strings.TrimSpace(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if role != "" && content != "" {
			messages = append(messages, common.ChatMessage{Role: role, Content: content})
		}
	}

	chunks, err := streamProvider.CallChatStream(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("start voice stream: %w", err)
	}

	var fullText strings.Builder
	var sentenceBuf strings.Builder
	sentenceIndex := 0
	streamStart := time.Now()
	firstSentenceLogged := false

	emit := func(text string, isFinal bool) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if !firstSentenceLogged {
			c.Logger.Info("voice trace: first sentence dispatched",
				"voiceTrace", "voice:"+spaceId,
				"stage", "cognition.sentence.first",
				"spaceId", spaceId,
				"chars", len(text),
				"firstSentenceMs", time.Since(streamStart).Milliseconds(),
			)
			firstSentenceLogged = true
		}
		// Initiative C, Phase 11: the bridge-agent sentence-stream
		// path was deleted. The voice-agent's memql LLM plugin gets
		// the prose via VoiceAgentTurnDelta on the existing gRPC
		// stream; per-sentence chunking is no longer needed here.
		_ = isFinal
		_ = text
		_ = winnerAgentName
		_ = winnerVoiceModel
		sentenceIndex++
	}

loop:
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				break loop
			}
			if chunk.Error != nil {
				err = chunk.Error
				break loop
			}
			if chunk.Done {
				break loop
			}
			if chunk.Content == "" {
				continue
			}
			fullText.WriteString(chunk.Content)
			sentenceBuf.WriteString(chunk.Content)
			// Flush every COMPLETE sentence we can find in the buffer.
			// The split keeps the trailing fragment in the buffer for
			// the next chunk to complete.
			remainder := flushCompleteSentences(&sentenceBuf, emit)
			sentenceBuf.Reset()
			sentenceBuf.WriteString(remainder)
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		}
	}

	// Flush whatever's left as the FINAL sentence (no terminating
	// punctuation needed -- end of stream means end of reply).
	tail := strings.TrimSpace(sentenceBuf.String())
	if tail != "" {
		emit(tail, true)
	}

	return fullText.String(), err
}

// flushCompleteSentences drains the buf of every COMPLETE sentence
// it can find, calling emit(sentence, isFinal=false) for each. The
// trailing un-terminated fragment is returned so the caller can
// re-prime the buffer with it.
func flushCompleteSentences(buf *strings.Builder, emit func(string, bool)) string {
	text := buf.String()
	var pending []byte
	out := text
	for {
		end, found := nextSentenceEnd(out)
		if !found {
			break
		}
		sentence := strings.TrimSpace(out[:end])
		if sentence != "" {
			emit(sentence, false)
		}
		out = out[end:]
		_ = pending // appease linters
	}
	return out
}

// nextSentenceEnd returns (offset, true) where offset is the index in
// text immediately past the next sentence-terminator + whitespace, or
// (0, false) if no complete sentence is in text yet.
//
// Sentence terminators: `.`, `?`, `!`, optionally followed by quotes
// or closing parens, then whitespace. A double-newline also counts so
// markdown paragraph breaks emit cleanly.
func nextSentenceEnd(text string) (int, bool) {
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '.', '?', '!':
			// Skip trailing closing punctuation.
			j := i + 1
			for j < len(text) && (text[j] == ')' || text[j] == ']' || text[j] == '"' || text[j] == '\'') {
				j++
			}
			// Need whitespace after to commit the boundary.
			if j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n') {
				// Skip the whitespace too.
				for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n') {
					j++
				}
				return j, true
			}
		case '\n':
			// Double newline = paragraph break = sentence boundary.
			if i+1 < len(text) && text[i+1] == '\n' {
				return i + 2, true
			}
		}
	}
	return 0, false
}

// bareSpaceSlug returns the bare-slug form of a space id (the
// portion after the last colon). The bridge's per-space room
// registry is keyed on whatever was passed to /join-room when the
// room was created -- in practice, the bare slug (`daily-XXX-DATE`)
// the frontend submitted. After 439ed91 every utterance row's
// payload.spaceId is canonical (`default:v1:cognition:space:daily-...`),
// so cognition reads canonical and would otherwise send canonical
// to the bridge, missing the registry key. Strip here right before
// every outbound bridge POST.
//
// Tolerant of inputs that are already bare slugs -- if there's no
// colon, returns the input unchanged.
func bareSpaceSlug(spaceId string) string {
	if idx := strings.LastIndex(spaceId, ":"); idx >= 0 {
		return spaceId[idx+1:]
	}
	return spaceId
}
