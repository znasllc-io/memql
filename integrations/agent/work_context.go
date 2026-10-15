package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
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
		reserve, err := workContextRecoveryReserve(ctx, nil)
		if err != nil {
			return nil, err
		}
		ctx = memql.ContextWithWorkContextRecoveryReserve(ctx, reserve)
		return engine.CompactWorkContext(ctx, messages, tools, target)
	}
	return messages, nil
}

func workContextRecoveryReserve(ctx context.Context, load workflowhost.Loader) (int, error) {
	value, err := workflowhost.Run(ctx, "agentWorkContextRecovery", nil, workflowhost.Options{Load: load})
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(value)
	var reserve int
	if err != nil || json.Unmarshal(raw, &reserve) != nil || reserve < 0 || value == nil {
		return 0, fmt.Errorf("context recovery policy must return a nonnegative whole-call reserve")
	}
	return reserve, nil
}
