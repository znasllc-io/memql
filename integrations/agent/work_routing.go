package agent

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// Classification belongs to the durable run. Read it on the executing replica,
// under the owner, rather than trusting a client hint or process-local cache.
// This chooses a routing level, never a provider; pins and policy still win.
func (r *Replier) workRoutingLevel(ctx context.Context) (airoute.Level, error) {
	if !isOwnedWorkExecution(ctx) {
		return airoute.LevelStrong, nil
	}
	run, _ := common.RunFromContext(ctx)
	call, err := parser.RenderCall("work.workRunForOwner", map[string]any{"runId": run.RunId})
	if err != nil {
		return "", err
	}
	result, err := r.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return "", fmt.Errorf("read work classification: %w", err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 1 {
		classification, _ := rows[0]["classification"].(map[string]any)
		if classification["workload"] == "lookup" {
			return airoute.LevelFast, nil
		}
	}
	return airoute.LevelStrong, nil
}
