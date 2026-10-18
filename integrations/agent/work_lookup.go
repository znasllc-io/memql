package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Supply the same authorized, bounded discovery the model would otherwise
// spend a turn requesting. These are contracts, never cached answer rows.
// Read classification on this replica; client hints cannot select this path.
func (r *Replier) workLookupContracts(ctx context.Context) string {
	if !isOwnedWorkExecution(ctx) {
		return ""
	}
	ctx = memql.ContextWithFreshRead(ctx)
	run, _ := common.RunFromContext(ctx)
	call, err := parser.RenderCall("work.workRunForOwner", map[string]any{"runId": run.RunId})
	if err != nil {
		return ""
	}
	result, err := r.engine.Execute(ctx, "query "+call)
	if err != nil {
		return ""
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		return ""
	}
	classification, _ := rows[0]["classification"].(map[string]any)
	if classification["workload"] != "lookup" {
		return ""
	}
	title, _ := classification["workTitle"].(string)
	// A generated display title can mostly be a filename or identifier. Its
	// words describe the object, not the operation (for example a filename
	// containing "capability-check" must not select permission checks).
	// Prefer the owner's bounded request, read on this executing replica.
	goalCall, goalErr := parser.RenderCall("work.workGoalForOwner", map[string]any{"goalId": run.GoalId})
	if goalErr == nil {
		if goalResult, readErr := r.engine.Execute(ctx, "query "+goalCall); readErr == nil {
			goals := memql.MaterializeRows(goalResult)
			if len(goals) == 1 {
				statement, _ := goals[0]["statement"].(string)
				if statement = strings.TrimSpace(statement); statement != "" && len(statement) <= 300 {
					title = statement
				}
			}
		}
	}
	// The classifier already understands the current question. Its durable
	// action/object search avoids ranking a filename or incidental words like
	// "first" above the operation needed. Missing/invalid hints retain the
	// ordinary bounded fallback, with no extra inference call.
	if search, ok := classification["capabilitySearch"].(string); ok {
		if search = strings.TrimSpace(search); search != "" && len(search) <= 300 {
			title = search
		}
	}
	if strings.TrimSpace(title) == "" || len(title) > 300 {
		return ""
	}
	call, err = parser.RenderCall("work.workCapabilities", map[string]any{"search": title})
	if err != nil {
		return ""
	}
	result, err = r.engine.Execute(ctx, "query "+call)
	if err != nil {
		return ""
	}
	rows = memql.MaterializeRows(result)
	if len(rows) == 0 {
		return ""
	}
	// MaterializeRows exposes payload fields at the top level while retaining
	// the original payload. Supplying both doubles the same contracts and
	// Fleet snapshot in every lookup prompt. Keep the discovery contract once;
	// storage envelopes and their duplicate payload are not model evidence.
	contracts := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		contract := map[string]any{}
		for _, key := range []string{"capabilities", "concepts", "fleet", "matchedCapabilityCount", "note", "truncated"} {
			if value, present := row[key]; present {
				contract[key] = value
			}
		}
		contracts = append(contracts, contract)
	}
	encoded, err := json.Marshal(contracts)
	if err != nil || len(encoded) > 24*1024 {
		return "" // The ordinary discovery tool remains available.
	}
	return string(encoded)
}
