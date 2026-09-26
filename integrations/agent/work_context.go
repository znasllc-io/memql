package agent

import (
	"context"
	"github.com/znasllc-io/memql/core/common"
)

// Shared work contexts reserve room for tool contracts and output. The router
// also sizes the actual call and chooses an eligible provider; overflow recovery
// can request a smaller checkpoint rather than silently dropping old turns.
func (r *Replier) compactWorkContext(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition, target int) ([]common.ChatMessage, error) {
	if !isOwnedWorkExecution(ctx) {
		return messages, nil
	}
	if engine, ok := r.engine.(interface {
		CompactWorkContext(context.Context, []common.ChatMessage, []common.ToolDefinition, int) ([]common.ChatMessage, error)
	}); ok {
		return engine.CompactWorkContext(ctx, messages, tools, target)
	}
	return messages, nil
}
