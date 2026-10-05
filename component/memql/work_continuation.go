package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// SaveWorkContinuation is a barrier before asking a person. It keeps the exact
// completed tool exchanges; resumption does not reconstruct effects from prose.
func (e *MemQLEngine) SaveWorkContinuation(ctx context.Context, messages []common.ChatMessage) error {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return fmt.Errorf("continuation requires owned work")
	}
	call, err := parser.RenderCall("updateWorkRun", map[string]any{"runId": run.RunId, "continuation": map[string]any{"stepKey": run.StepKey, "messages": messages, "at": time.Now().UTC()}})
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

func (e *MemQLEngine) RestoreWorkContinuation(ctx context.Context, messages []common.ChatMessage) ([]common.ChatMessage, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.GoalId == "" || run.Mode == common.RunModeReplay || !run.Override.Empty() {
		return messages, nil
	}
	rows, err := e.workRows(ContextWithFreshRead(ctx), "workRunForOwner", run.RunId)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("work continuation is unavailable")
	}
	saved, _ := rows[0]["continuation"].(map[string]any)
	if saved["stepKey"] != run.StepKey {
		return messages, nil
	}
	raw, err := json.Marshal(saved["messages"])
	if err != nil {
		return nil, err
	}
	var prior []common.ChatMessage
	if err = json.Unmarshal(raw, &prior); err != nil {
		return nil, err
	}
	if len(prior) == 0 {
		return messages, nil
	}
	// Keep current instructions and the person's newly recorded answers. The
	// saved tail already contains the original request and completed results.
	for _, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this ") {
			prior = append(prior, m)
		}
	}
	return prior, nil
}
