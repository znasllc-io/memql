package goaleval

import (
	"context"
	"fmt"
)

// EvidenceReader reads owned rows through the ordinary work API. Its adapter
// supplies wire-form IDs; the evaluator never constructs or parses node IDs.
type EvidenceReader func(context.Context, string, string) ([]map[string]any, error)

type RunEvidence struct {
	Runs         []map[string]any `json:"runs"`
	ModelCalls   []map[string]any `json:"modelCalls"`
	Observations []map[string]any `json:"observations"`
	Steps        []map[string]any `json:"steps"`
}

// CollectRunEvidence follows durable step-to-child links. It refuses missing,
// live, foreign-parent or differently routed children instead of presenting a
// root-only model list as evidence about the whole goal.
func CollectRunEvidence(ctx context.Context, root, source string, read EvidenceReader) (RunEvidence, error) {
	var evidence RunEvidence
	type link struct{ id, parent string }
	queue := []link{{id: root}}
	seen := map[string]string{}
	text := func(row map[string]any, key string) string { s, _ := row[key].(string); return s }
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if parent, ok := seen[item.id]; ok {
			if parent != item.parent {
				return evidence, fmt.Errorf("evaluation child has inconsistent parents")
			}
			continue
		}
		if len(seen) >= 64 {
			return evidence, fmt.Errorf("evaluation run family exceeds the evidence limit")
		}
		seen[item.id] = item.parent
		rows, err := read(ctx, "run", item.id)
		if err != nil {
			return evidence, err
		}
		if len(rows) != 1 || text(rows[0], "id") != item.id || (item.parent != "" && text(rows[0], "parentRunId") != item.parent) {
			return evidence, fmt.Errorf("evaluation child run is missing or does not belong to its parent")
		}
		run := rows[0]
		routing, _ := run["routing"].(map[string]any)
		if text(routing, "source") != source || routing["noPaidProviders"] != true {
			return evidence, fmt.Errorf("evaluation run family lost its no-paid-provider fleet pin")
		}
		switch text(run, "status") {
		case "succeeded", "failed", "cancelled", "abandoned":
		default:
			return evidence, fmt.Errorf("evaluation run family still has active work")
		}
		evidence.Runs = append(evidence.Runs, run)
		for _, kind := range []string{"modelCalls", "observations", "steps"} {
			rows, err := read(ctx, kind, item.id)
			if err != nil {
				return evidence, err
			}
			switch kind {
			case "modelCalls":
				evidence.ModelCalls = append(evidence.ModelCalls, rows...)
			case "observations":
				evidence.Observations = append(evidence.Observations, rows...)
			case "steps":
				evidence.Steps = append(evidence.Steps, rows...)
				for _, row := range rows {
					if child := text(row, "childRunId"); child != "" {
						queue = append(queue, link{id: child, parent: item.id})
					}
				}
			}
		}
	}
	return evidence, nil
}
