package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// maxStreamingToolLoopIterations caps the number of model turns in a streaming
// tool-calling loop. Each iteration is one round-trip to the model; tool
// results from the previous iteration are appended as `Role: "tool"` messages
// before the next call. Three is enough for "acknowledgment + tool lookup +
// follow-up tool + final reply" without runaway latency.
const maxStreamingToolLoopIterations = 3

// runStreamingToolLoop drives a bounded multi-turn streaming tool-calling loop.
//
// Text content streams to the frontend in real time as it arrives. After each
// turn, any tool calls requested by the model are executed and their results
// are fed back as `Role: "tool"` messages so the next turn can continue the
// reply using the retrieved data. The text chunk index is monotonic across
// turns so the frontend renders one continuous reply rather than disjoint
// fragments.
//
// The loop terminates when the model returns text without tool calls, when
// every tool in a round errored (nothing useful to feed back), when the
// iteration cap is hit, or when a stream error occurs. In every case, a final
// `done=true` text-chunk event is emitted.
//
// replyId is the stable per-turn id stamped on every emitted chunk and
// reused as the committed utterance.id so the streaming bubble and the
// final committed bubble share one React key.
func (c *CognitionIntegration) runStreamingToolLoop(
	ctx context.Context,
	streamProvider common.ChatStreamWithToolsProvider,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
	partitionId, participantId, replyId string,
	logTag string,
) (*streamResult, error) {
	start := time.Now()

	var fullText strings.Builder
	textChunkIndex := 0
	var allToolCalls []common.ToolCall

	for iter := 0; iter < maxStreamingToolLoopIterations; iter++ {
		chunks, err := streamProvider.CallChatStreamWithTools(ctx, messages, tools)
		if err != nil {
			if iter == 0 {
				return nil, fmt.Errorf("start stream: %w", err)
			}
			c.Logger.Warn(logTag+": continuation stream failed",
				"iter", iter, "error", err)
			break
		}

		turnText, turnCalls, streamErr := c.consumeStreamingTurn(
			ctx, chunks, partitionId, participantId, replyId, &textChunkIndex, &fullText)
		if streamErr != nil {
			c.Logger.Warn(logTag+": stream error",
				"iter", iter, "error", streamErr)
			break
		}

		assistantMsg := common.ChatMessage{Role: "assistant", Content: turnText}
		if len(turnCalls) > 0 {
			assistantMsg.ToolCalls = turnCalls
		}
		messages = append(messages, assistantMsg)

		if len(turnCalls) == 0 {
			break
		}
		allToolCalls = append(allToolCalls, turnCalls...)

		if iter == maxStreamingToolLoopIterations-1 {
			// Budget exhausted. Execute the final round's tools for their
			// side effects (e.g. clawExecuteTask kicks off a task) but don't
			// feed results back — there's no turn left to consume them.
			for _, tc := range turnCalls {
				args := parseToolArgs(tc.Arguments)
				if _, execErr := c.engine.ExecuteToolByName(ctx, tc.Name, args); execErr != nil {
					c.Logger.Warn(logTag+": tool execution failed",
						"tool", tc.Name, "error", execErr)
				}
			}
			break
		}

		hadSuccess := false
		for _, tc := range turnCalls {
			args := parseToolArgs(tc.Arguments)
			result, execErr := c.engine.ExecuteToolByName(ctx, tc.Name, args)
			var content string
			if execErr != nil {
				c.Logger.Warn(logTag+": tool execution failed",
					"tool", tc.Name, "error", execErr)
				content = fmt.Sprintf(`{"error":%q}`, execErr.Error())
			} else {
				content = result
				hadSuccess = true
			}
			messages = append(messages, common.ChatMessage{
				Role:       "tool",
				Name:       tc.Name,
				ToolCallId: tc.ID,
				Content:    content,
			})
		}

		if !hadSuccess {
			// Every tool in this round errored — looping back would just ask
			// the model to apologize with no new information. Stop cleanly.
			break
		}
	}

	c.emitTextChunk(ctx, partitionId, participantId, replyId, "", textChunkIndex, true)

	finalText := strings.TrimSpace(fullText.String())
	c.Logger.Info(logTag+": complete",
		"partitionId", partitionId,
		"textChunks", textChunkIndex,
		"textChars", len(finalText),
		"toolCalls", len(allToolCalls),
		"totalMs", time.Since(start).Milliseconds(),
	)

	return &streamResult{
		Text:       finalText,
		TextChunks: textChunkIndex,
		ToolCalls:  allToolCalls,
	}, nil
}

// consumeStreamingTurn reads a single turn from the provider's channel,
// emitting text chunks as they cross flush boundaries and accumulating any
// tool-call deltas. textChunkIndex is advanced in place so callers can pass
// it straight into the next turn for continuous numbering, and fullText
// accumulates across turns so the returned streamResult.Text contains the
// entire reply.
//
// replyId is forwarded through to emitTextChunk -- see runStreamingToolLoop.
func (c *CognitionIntegration) consumeStreamingTurn(
	ctx context.Context,
	chunks <-chan common.StreamToolChunk,
	partitionId, participantId, replyId string,
	textChunkIndex *int,
	fullText *strings.Builder,
) (turnText string, toolCalls []common.ToolCall, err error) {
	var turnBuilder strings.Builder
	var textBuffer strings.Builder

	type toolCallAccum struct {
		id   string
		name string
		args strings.Builder
	}
	toolAccum := make(map[int]*toolCallAccum)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	flush := func() {
		if textBuffer.Len() > 0 {
			c.emitTextChunk(ctx, partitionId, participantId, replyId, textBuffer.String(), *textChunkIndex, false)
			textBuffer.Reset()
			*textChunkIndex++
			ticker.Reset(2 * time.Second)
		}
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
			if chunk.Content != "" {
				fullText.WriteString(chunk.Content)
				turnBuilder.WriteString(chunk.Content)
				textBuffer.WriteString(chunk.Content)
				if shouldFlushTextBuffer(textBuffer.String()) {
					flush()
				}
			}
			for _, tcd := range chunk.ToolCalls {
				acc, exists := toolAccum[tcd.Index]
				if !exists {
					acc = &toolCallAccum{}
					toolAccum[tcd.Index] = acc
				}
				if tcd.ID != "" {
					acc.id = tcd.ID
				}
				if tcd.Name != "" {
					acc.name = tcd.Name
				}
				acc.args.WriteString(tcd.Arguments)
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			break loop
		}
	}

	flush()
	turnText = strings.TrimSpace(turnBuilder.String())

	if len(toolAccum) > 0 {
		indices := make([]int, 0, len(toolAccum))
		for idx := range toolAccum {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		toolCalls = make([]common.ToolCall, 0, len(indices))
		for _, idx := range indices {
			acc := toolAccum[idx]
			toolCalls = append(toolCalls, common.ToolCall{
				ID:        acc.id,
				Name:      acc.name,
				Arguments: strings.TrimSpace(acc.args.String()),
			})
		}
	}

	return
}

func parseToolArgs(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(raw), &args)
	return args
}
