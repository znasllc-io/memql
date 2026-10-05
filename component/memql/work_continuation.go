package memql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

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
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	// Append a receipt rather than read-merging the run row. Heartbeats,
	// cancellation and answers can arrive on another replica during this write.
	call, err := parser.RenderCall("createWorkObservation", map[string]any{
		"observationId": fmt.Sprintf("continuation-%s-%x", BareShortId(run.RunId), sha256.Sum256(append([]byte(run.StepKey), raw...))),
		"runId":         run.RunId, "stepKey": run.StepKey, "kind": "note", "content": "Saved execution progress",
		"data": map[string]any{"continuation": true, "messages": messages},
	})
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
	call, err := parser.RenderCall("workContinuationForOwnerRun", map[string]any{"runId": run.RunId, "stepKey": run.StepKey})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	checkpoints := MaterializeRows(result.OutputPayload())
	if len(checkpoints) == 0 {
		return messages, nil
	}
	saved, _ := checkpoints[0]["data"].(map[string]any)
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
	// Refresh instructions/profile context rather than resurrecting the system
	// prompt captured by a previous replica or before a profile change.
	if len(messages) > 0 && messages[0].Role == "system" && prior[0].Role == "system" {
		prior[0] = messages[0]
	}
	// Keep current instructions and the person's newly recorded answers. The
	// saved tail already contains the original request and completed results.
	for _, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this ") {
			found := false
			for _, previous := range prior {
				if previous.Role == m.Role && previous.Content == m.Content {
					found = true
					break
				}
			}
			if !found {
				prior = append(prior, m)
			}
		}
	}
	return prior, nil
}

// PrepareWorkTool revises only a classifier estimate, through the work
// integration's durable writer. It never grants permission or changes a goal.
func (e *MemQLEngine) PrepareWorkTool(ctx context.Context) error {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if !ok || ac == nil || BareShortId(ac.UserId) != BareShortId(run.OwnerUserId) {
		return fmt.Errorf("workload transition requires owned work")
	}
	writer, ok := e.IntegrationByName("work").(interface {
		PrepareWorkTool(context.Context) (bool, error)
	})
	if !ok {
		return fmt.Errorf("workload coordination is unavailable")
	}
	_, err := writer.PrepareWorkTool(ctx)
	return err
}
