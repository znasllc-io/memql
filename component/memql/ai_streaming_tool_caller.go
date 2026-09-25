package memql

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// Adapt the streaming provider to the existing bounded, permission-checked
// tool loop. A closed or canceled stream is never a successful tool request.
type streamingToolCaller struct {
	provider common.ChatStreamWithToolsProvider
	onText   func(string)
}

func (s streamingToolCaller) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	chunks, err := s.provider.CallChatStreamWithTools(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	result := &common.ToolCallingChatResult{}
	calls := map[int]*common.ToolCall{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, open := <-chunks:
			if !open {
				return nil, fmt.Errorf("model stream closed before completion")
			}
			if chunk.Error != nil {
				return nil, chunk.Error
			}
			if chunk.Content != "" {
				result.AssistantText += chunk.Content
				s.onText(chunk.Content)
			}
			for _, delta := range chunk.ToolCalls {
				if delta.Index < 0 || delta.Index >= 64 {
					return nil, fmt.Errorf("invalid tool call index")
				}
				call := calls[delta.Index]
				if call == nil {
					call = &common.ToolCall{}
					calls[delta.Index] = call
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				if delta.Name != "" {
					call.Name += delta.Name
				}
				call.Arguments += delta.Arguments
				if len(call.Arguments) > 1024*1024 {
					return nil, fmt.Errorf("tool arguments exceed limit")
				}
			}
			if chunk.Done {
				indexes := make([]int, 0, len(calls))
				for index := range calls {
					indexes = append(indexes, index)
				}
				sort.Ints(indexes)
				for _, index := range indexes {
					result.ToolCalls = append(result.ToolCalls, *calls[index])
				}
				return result, nil
			}
		}
	}
}

func streamTextOnlyTurn(ctx context.Context, resolved ResolvedProvider, system string, history []map[string]any, opts *ToolLoopOptions) (answer string, err error) {
	provider, ok := resolved.Client.(common.ChatStreamProvider)
	if !ok {
		return "", fmt.Errorf("selected route cannot stream a response")
	}
	messages := []common.ChatMessage{{Role: "system", Content: system + "\nThis route has no tool calling. Answer questions from available context; explain that actions require a tool-capable model. Do not claim to have read or changed the installation."}}
	for _, item := range history {
		role, _ := item["role"].(string)
		content, _ := item["content"].(string)
		if (role == "user" || role == "assistant") && content != "" {
			messages = append(messages, common.ChatMessage{Role: role, Content: content})
		}
	}
	started := time.Now()
	if opts.OnModel != nil {
		opts.OnModel("running", resolved.Resolution.ProviderName, resolved.Resolution.Model, 0, nil)
		defer func() {
			opts.OnModel("completed", resolved.Resolution.ProviderName, resolved.Resolution.Model, time.Since(started), err)
		}()
	}
	chunks, err := provider.CallChatStream(ctx, messages)
	if err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return answer, ctx.Err()
		case chunk, open := <-chunks:
			if !open {
				return answer, fmt.Errorf("model stream ended before completion")
			}
			if chunk.Error != nil {
				return answer, chunk.Error
			}
			if chunk.Content != "" {
				answer += chunk.Content
				opts.OnText(chunk.Content)
			}
			if chunk.Done {
				return answer, nil
			}
		}
	}
}
