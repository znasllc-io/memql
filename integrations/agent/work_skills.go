package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func (r *Replier) workSkillContext(ctx context.Context, stepText string) ([]map[string]any, error) {
	if !isOwnedWorkExecution(ctx) {
		return nil, nil
	}
	rc, _ := common.RunFromContext(ctx)
	if rc.StepKey == "" || (rc.Mode != common.RunModeLive && rc.Mode != common.RunModeFork && rc.Mode != common.RunModeReplay) {
		return nil, nil
	}
	value, err := workflowhost.Run(ctx, "agentWorkSkillContext", map[string]any{"text": stepText}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"agentSelectWorkSkills": func(ctx context.Context, args map[string]any) (any, error) {
			// The native capability derives the run, owner, step and content from
			// trusted context and current rows. This recipe supplies only limits.
			query := fmt.Sprintf("builtin workSelectSkills(limit: %s, minimumScore: %s, maxBytes: %s, text: %s)", jsonValue(args["limit"]), jsonValue(args["minimumScore"]), jsonValue(args["maxBytes"]), jsonValue(args["text"]))
			result, err := r.engine.Execute(ctx, query)
			if err != nil {
				return nil, err
			}
			rows := memql.MaterializeRows(result)
			if len(rows) != 1 {
				return nil, fmt.Errorf("skill context receipt is unavailable")
			}
			return rows[0], nil
		},
	}})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var receipt struct {
		ReceiptID string `json:"receiptId"`
		Selection struct {
			Selected []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Version int    `json:"version"`
				Digest  string `json:"digest"`
				Text    string `json:"text"`
			} `json:"selected"`
		} `json:"selection"`
	}
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return nil, err
	}
	if receipt.ReceiptID == "" {
		return nil, fmt.Errorf("skill context receipt is missing")
	}
	out := make([]map[string]any, 0, len(receipt.Selection.Selected))
	for _, c := range receipt.Selection.Selected {
		out = append(out, map[string]any{"text": c.Text, "citation": fmt.Sprintf("Skill %s v%d (%s, receipt %s, sha256 %s)", c.Name, c.Version, c.ID, receipt.ReceiptID, c.Digest)})
	}
	return out, nil
}

func jsonValue(v any) string { b, _ := json.Marshal(v); return string(b) }
