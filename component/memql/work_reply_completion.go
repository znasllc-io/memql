package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

type replyTaskKey struct{}

// The native runAgentTurn input names this step's task. It is context, never
// authority: the reader still checks the owned execution and current guidance.
// Keep it separate from an assistant's proposed answer or a restored summary.
func ContextWithWorkReplyTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, replyTaskKey{}, task)
}

// Presence is set by native production, including specialist turns without a
// human-facing terminal tool. Direct quick-reply builtins have no such task.
func HasWorkReplyTask(ctx context.Context) bool {
	_, present := ctx.Value(replyTaskKey{}).(string)
	return present
}

func (e *MemQLEngine) replyCompletionScope(ctx context.Context, requested bool) (string, error) {
	if !requested {
		return "", nil
	}
	task, _ := ctx.Value(replyTaskKey{}).(string)
	if !HasWorkReplyTask(ctx) {
		return "", fmt.Errorf("reply completion requires the current step task context")
	}
	run, _ := common.RunFromContext(ctx)
	rows, err := e.memoryCall(ContextWithFreshRead(ctx), "query", "work.workStepForOwnerRun", map[string]any{"runId": run.RunId, "stepKey": run.StepKey})
	if err != nil || len(rows) != 1 || BareShortId(stringArg(rows[0], "ownerUserId")) != BareShortId(run.OwnerUserId) || BareShortId(stringArg(rows[0], "runId")) != BareShortId(run.RunId) || stringArg(rows[0], "key") != run.StepKey || rows[0]["status"] != "running" {
		return "", fmt.Errorf("reply completion current step unavailable: %v", err)
	}
	if strings.TrimSpace(task) == "" {
		// Adopted root turns may carry a large source document as their latest
		// user message. Their recorded goal, not that document, names the task.
		goals, readErr := e.memoryCall(ContextWithFreshRead(ctx), "query", "work.workGoalForOwner", map[string]any{"goalId": run.GoalId})
		if readErr != nil || len(goals) != 1 || BareShortId(stringArg(goals[0], "ownerUserId")) != BareShortId(run.OwnerUserId) {
			return "", fmt.Errorf("reply completion task unavailable: %v", readErr)
		}
		task = stringArg(goals[0], "statement")
	}
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("reply completion task is empty")
	}
	// Never clip or reject a long native instruction at an invented text limit.
	// Normal routed context admission and durable recovery bound the model call.
	raw, err := json.Marshal(map[string]any{"stepKey": run.StepKey, "task": task, "call": rows[0]["call"], "postcondition": rows[0]["postcondition"]})
	if err != nil {
		return "", fmt.Errorf("reply completion step contract: %w", err)
	}
	return string(raw), nil
}

// The reviewer distinguishes this step's information request from an operation.
// Source references must name actual retained previews; this remains a model
// pre-filter, not a transactional certification that an effect occurred.
type WorkReplyCompletion struct {
	Kind        string   `json:"kind"`
	Disposition string   `json:"disposition"`
	Gap         string   `json:"gap"`
	SourceRefs  []string `json:"sourceRefs"`
}

func (c *WorkReplyCompletion) Validate(allowed map[string]bool) error {
	if c == nil || (c.Kind != "information" && c.Kind != "operation") || (c.Disposition != "satisfied" && c.Disposition != "unfinished") || len(c.Gap) > 1200 || len(c.SourceRefs) > 16 {
		return fmt.Errorf("reply completion returned an invalid disposition")
	}
	if c.Kind == "information" && (c.Disposition != "satisfied" || c.Gap != "") {
		return fmt.Errorf("an informational answer is not an unfinished operation")
	}
	if c.Disposition == "unfinished" && strings.TrimSpace(c.Gap) == "" {
		return fmt.Errorf("unfinished operation requires a bounded gap")
	}
	if c.Disposition == "satisfied" && c.Gap != "" {
		return fmt.Errorf("satisfied step has an unfinished operation gap")
	}
	if c.Kind == "operation" && c.Disposition == "satisfied" && len(c.SourceRefs) == 0 {
		return fmt.Errorf("operation completion requires a recorded source")
	}
	for _, ref := range c.SourceRefs {
		if ref == "" || len(ref) > 200 || (allowed != nil && !allowed[ref]) {
			return fmt.Errorf("reply completion cites an unavailable source")
		}
	}
	return nil
}

func replyCompletionSources(sources []any, runID string) map[string]bool {
	refs := map[string]bool{}
	for n, source := range sources {
		row, ok := source.(map[string]any)
		if !ok {
			continue
		}
		// Preview references are local to this bounded check, never authority or
		// stable journal IDs. Foreign-run context cannot certify this operation.
		ref := fmt.Sprintf("source-%d", n+1)
		row["reviewSourceRef"] = ref
		if BareShortId(stringArg(row, "runId")) == BareShortId(runID) && stringArg(row, "stepKey") != "" && operationOutcomeEligible(row) {
			refs[ref] = true
		}
	}
	return refs
}

// Refuse explicit error/omission envelopes mechanically. Semantic relevance and
// whether a prior step's retained artifact satisfies this task remain the
// bounded reviewer's judgment; an eligible receipt is not itself completion.
func operationOutcomeEligible(row map[string]any) bool {
	switch stringArg(row, "operation") {
	case "workViewerContext", "requestComputerUseScope", "requestUserFeedback":
		return false
	}
	var incomplete func(any) bool
	incomplete = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			for _, key := range []string{"isError", "truncated", "sourceOmitted", "resultOmitted"} {
				if v[key] == true {
					return true
				}
			}
			if v["ok"] == false || v["success"] == false {
				return true
			}
			if err, exists := v["error"]; exists && err != nil && err != "" && err != false {
				return true
			}
			if code, exists := v["exitCode"]; exists && fmt.Sprint(code) != "0" {
				return true
			}
			// Only recognized MCP transport text envelopes are recursively decoded.
			// Domain payloads may describe failed tests or contain error fields in
			// a successfully saved report; those are not tool execution failures.
			if blocks, ok := v["content"].([]any); ok {
				for _, raw := range blocks {
					if block, ok := raw.(map[string]any); ok && block["type"] == "text" {
						if incomplete(block["textJSON"]) {
							return true
						}
					}
				}
			}
		}
		return false
	}
	return !incomplete(row["value"])
}
