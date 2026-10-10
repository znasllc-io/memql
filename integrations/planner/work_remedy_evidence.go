package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

// This is a prompt projection, never the journal used for execution or prefix
// validation. Large fan-in values often repeat every earlier chapter, while a
// step's legacy result envelope can repeat its own value again.
func replanEvidenceView(ctx context.Context, receipts []map[string]any) ([]map[string]any, error) {
	value, err := workflowhost.Run(ctx, "workReplanEvidencePolicy", nil, workflowhost.Options{})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var limits struct{ PerStepCharacters, TotalCharacters int }
	if err = json.Unmarshal(raw, &limits); err != nil || limits.PerStepCharacters < 1 || limits.TotalCharacters < limits.PerStepCharacters || limits.TotalCharacters > 1<<20 {
		return nil, fmt.Errorf("invalid replan evidence limits")
	}
	return projectReplanEvidence(receipts, limits.PerStepCharacters, limits.TotalCharacters)
}

func projectReplanEvidence(receipts []map[string]any, perStep, remaining int) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(receipts))
	for _, receipt := range receipts {
		// Never drop a completed identity, even after the preview allowance is used.
		entry := map[string]any{"key": receipt["key"], "call": receipt["call"]}
		result := receipt["result"]
		if envelope, ok := result.(map[string]any); ok {
			if value, exists := envelope["value"]; exists {
				result = value
			}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("cannot encode completed step %v: %w", receipt["key"], err)
		}
		text := []rune(string(raw))
		allowance := min(perStep, remaining)
		if len(text) <= allowance {
			entry["result"] = result
			remaining -= len(text)
		} else {
			digest := sha256.Sum256(raw)
			head := allowance / 2
			tail := allowance - head
			entry["resultPreview"] = string(text[:head]) + "\n[stored result omitted]\n" + string(text[len(text)-tail:])
			entry["resultCharacters"] = len(text)
			entry["resultSHA256"] = hex.EncodeToString(digest[:])
			entry["resultOmitted"] = true
			remaining -= allowance
		}
		out = append(out, entry)
	}
	return out, nil
}
