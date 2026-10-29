package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// work_call_tags.go -- the call tags a work run's model calls carry because of
// where the run's goal came from (spec 2026-10-29, D5).
//
// A goal a pipeline opened -- a failed run's diagnosis, the repository change
// it requests -- records `requestedVia: "pipeline"` on its row. Every model
// call its run makes carries the `pipeline` tag, so the shipped rule
// pipelineLocalFirst routes it to the owner's machines first, then an app,
// then a vendor, in that order, instead of reasoningParks' chain that Auto
// re-ranks with an app first.
//
// THE TAG IS READ ON THE REPLICA EXECUTING THE RUN, off the goal row, never
// carried in memory from the replica that opened the goal: the run is claimed
// wherever a running event lands. A goal that cannot be read refuses nothing
// -- the run executes and routes by the ordinary rules, and the dispatcher
// logs why -- because the tag is a routing preference, not a gate.

// goalCallTags reads the goal under the owner the context already carries
// (workExecutionContext restored it) and returns its call tags.
func goalCallTags(ctx context.Context, execute func(context.Context, string) (*memql.ExecuteResult, error), goalId string) ([]string, error) {
	if strings.TrimSpace(goalId) == "" {
		return nil, nil
	}
	call, err := langparser.RenderCall("workGoalForOwner", map[string]any{"goalId": goalId})
	if err != nil {
		return nil, err
	}
	result, err := execute(ctx, "query "+call)
	if err != nil {
		return nil, fmt.Errorf("read goal %s: %w", goalId, err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return nil, nil
	}
	return workspine.GoalCallTags(rows[0]), nil
}

// withRunCallTags adds tags to the run context already on ctx, each once.
func withRunCallTags(ctx context.Context, tags []string) context.Context {
	if len(tags) == 0 {
		return ctx
	}
	run, ok := common.RunFromContext(ctx)
	if !ok {
		return ctx
	}
	merged := append([]string(nil), run.Tags...)
	for _, tag := range tags {
		if !slices.Contains(merged, tag) {
			merged = append(merged, tag)
		}
	}
	run.Tags = merged
	return common.ContextWithRun(ctx, run)
}
