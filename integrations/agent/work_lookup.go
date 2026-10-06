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
	encoded, err := json.Marshal(rows)
	if err != nil || len(encoded) > 24*1024 {
		return "" // The ordinary discovery tool remains available.
	}
	return string(encoded)
}
