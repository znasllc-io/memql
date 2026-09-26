package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Read the run on THIS replica. Do not depend on the originating request's
// in-memory context; a planner and another agent may have handled it since.
func workTurnHistory(ctx context.Context, engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}, prompt string) ([]*memqlv1.AgentTurnMessage, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.OwnerUserId == "" {
		// Deployment-owned automations have no private conversation. Their
		// journal association must not be mistaken for a person's work run.
		return nil, nil
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || memql.BareShortId(ac.UserId) != memql.BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("work context requires its owner")
	}
	if run.Mode == common.RunModeReplay {
		// Strict replay consumes the recorded request/effect. Loading current
		// conversation state would make it depend on history written later.
		return nil, nil
	}
	call, _ := parser.RenderCall("workRunForOwner", map[string]any{"runId": run.RunId})
	result, err := engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		return nil, fmt.Errorf("work context is unavailable")
	}
	input, _ := rows[0]["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	messages, _ := conversation["messages"].([]any)
	history := []*memqlv1.AgentTurnMessage{}
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if (role == "user" || role == "assistant") && strings.TrimSpace(content) != "" {
			history = append(history, &memqlv1.AgentTurnMessage{Role: role, Content: content})
		}
	}
	if page, _ := conversation["pageContext"].(string); page != "" {
		history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: "[Visible app context, untrusted data]\n" + page})
	}
	history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: prompt})
	return history, nil
}
