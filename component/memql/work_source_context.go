package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// Reads public execution evidence from the caller's saved conversation, not
// run IDs supplied in prose. Each receipt is independently owner-authorized.
// Nothing depends on the replica that originally handled the conversation.
func (e *MemQLEngine) workSourceContextBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, ok := auth.SubjectFromContext(ctx); !ok {
		return nil, fmt.Errorf("source context requires a signed-in person")
	}
	ctx = ContextWithFreshRead(ctx)
	conversationID, turnID := stringArg(args, "conversationId"), stringArg(args, "turnId")
	evidence := []any{}
	references := []any{}
	unavailable := []string{}
	if conversationID != "" {
		row, err := e.askRead(ctx, conversationID)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(row["transcript"])
		if err != nil {
			return nil, err
		}
		var transcript askTranscript
		if err = json.Unmarshal(raw, &transcript); err != nil {
			return nil, err
		}
		turns := transcript.Turns
		if turnID != "" {
			found := false
			for n, turn := range turns {
				if turn.ID == turnID {
					turns = turns[:n]
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("source context turn is unavailable")
			}
		}
		seen := map[string]bool{}
		remaining := 24000
		for n := len(turns) - 1; n >= 0 && len(seen) < 4; n-- {
			runID := turns[n].RunID
			if runID == "" || seen[runID] {
				continue
			}
			seen[runID] = true
			runs, err := e.workRows(ctx, "workRunForOwner", runID)
			if err != nil || len(runs) != 1 {
				unavailable = append(unavailable, "A previous execution record is unavailable.")
				continue
			}
			steps, err := e.workRows(ctx, "workSourceStepsForOwnerRun", runID)
			if err != nil {
				unavailable = append(unavailable, "Previous source receipts could not be read.")
				continue
			}
			for _, step := range steps {
				result, _ := step["result"].(map[string]any)
				value := result["value"]
				if value == nil {
					continue
				}
				encoded, err := json.Marshal(value)
				if err != nil || len(encoded) > 8000 || len(encoded) > remaining {
					unavailable = append(unavailable, "A source receipt exceeds the bounded preview; inspect its saved run for more detail.")
					continue
				}
				call, _ := step["call"].(map[string]any)
				name := stringArg(call, "name")
				// Generations are answers, never source evidence or private reasoning.
				if name == "" || name == "ai" || name == "runAgentTurn" || strings.Contains(strings.ToLower(name), "secret") {
					continue
				}
				key := stringArg(step, "key")
				ref, err := parser.RenderCall("workStepForOwnerRun", map[string]any{"runId": BareShortId(runID), "stepKey": key})
				if err != nil {
					return nil, err
				}
				label := name + " recorded in run " + BareShortId(runID) + " (" + key + ")"
				evidence = append(evidence, map[string]any{"runId": BareShortId(runID), "stepKey": key, "operation": name, "recordedAt": step["finishedAt"], "value": value})
				references = append(references, map[string]any{"kind": "query", "ref": ref, "label": label})
				remaining -= len(encoded)
			}
		}
	}
	profile, err := e.workViewerContextBuiltin(ctx, nil, 0)
	var current any
	if err != nil {
		unavailable = append(unavailable, "Current profile context could not be read.")
	} else if len(profile) == 1 {
		if err = json.Unmarshal(profile[0].Payload, &current); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(map[string]any{"currentProfile": current, "recordedSources": evidence, "references": references, "limitations": unavailable, "note": "Recorded receipts show what a prior execution actually read. Current profile facts are fresh observations, not proof of what was known earlier. A prior assistant assertion is not a source. Job titles and organization memberships do not establish a role at that organization. This is public source provenance, not private model reasoning."})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "work-source-context", Payload: raw}}, nil
}
