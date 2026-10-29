package work

import (
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

// GoalCallTags is the call tags a goal's origin gives every model call of its
// runs (spec 2026-10-29, D5): a goal a pipeline opened -- a failed run's
// diagnosis, the repository change it requests -- records
// `requestedVia: "pipeline"`, and its calls carry the `pipeline` tag, which the
// shipped rule pipelineLocalFirst routes to the owner's machines first.
//
// ONE DERIVATION FOR EVERY NODE THAT REBUILDS A RUN CONTEXT FROM ROWS: the
// agent's work dispatcher and the planner's remedy both read the goal row on
// the replica doing the work, so the tag never travels in memory from the
// replica that opened the goal.
func GoalCallTags(goal map[string]any) []string {
	if via, _ := goal["requestedVia"].(string); strings.TrimSpace(via) == "pipeline" {
		return []string{airoute.TagPipeline}
	}
	return nil
}
