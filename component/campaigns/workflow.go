package campaigns

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

// Delivery retains its native claim, consent checks, immutable reviewed bytes,
// throttle handling and receipts. This scope supplies those bounded operations
// to the campaign's authored maintenance and selection recipes.
func (w *Worker) drainWorkflow(ctx context.Context) error {
	systemCtx := w.systemActorContext(ctx)
	selected := map[string]SendJob{}
	_, err := workflowhost.Run(ctx, "campaignDrainWorkflow", nil, workflowhost.Options{Logger: w.logger, Operations: map[string]workflowhost.Operation{
		"campaignPromoteSeries": func(ctx context.Context, _ map[string]any) (any, error) {
			w.promoteDueSeries(ctx, systemCtx)
			return nil, nil
		},
		"campaignDrainWelcomes": func(ctx context.Context, _ map[string]any) (any, error) {
			w.drainNewsletterWelcomes(ctx, systemCtx)
			return nil, nil
		},
		"campaignPromoteSchedules": func(ctx context.Context, _ map[string]any) (any, error) {
			w.promoteDueSchedules(ctx, systemCtx)
			return nil, nil
		},
		"campaignApplyWarmup": func(ctx context.Context, _ map[string]any) (any, error) {
			w.applyWarmup(systemCtx, w.nowUTC())
			return nil, nil
		},
		"campaignDrainableJobs": func(ctx context.Context, _ map[string]any) (any, error) {
			jobs, err := w.store.DrainableJobs(systemCtx)
			if err != nil {
				return nil, err
			}
			ids := []string{}
			for _, job := range jobs {
				selected[job.ID] = job
				ids = append(ids, job.ID)
			}
			return ids, nil
		},
		"campaignDrainJob": func(ctx context.Context, a map[string]any) (any, error) {
			id, _ := a["id"].(string)
			job, ok := selected[id]
			if !ok {
				return nil, fmt.Errorf("campaign job outside drain snapshot")
			}
			w.processJob(ctx, systemCtx, job)
			return nil, nil
		},
		"campaignFlushReputation": func(ctx context.Context, _ map[string]any) (any, error) {
			w.flushReputation(systemCtx)
			return nil, nil
		},
	}})
	return err
}

// Pagination is a reader concern. Policy for each page uses the same DSL
// template and cannot dispatch an id that the current page did not contain.
func campaignPage(ctx context.Context, ids []string, operation func(context.Context, string) error) error {
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	_, err := workflowhost.Run(ctx, "campaignSelectedPageWorkflow", map[string]any{"ids": ids}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"campaignProcessSelected": func(ctx context.Context, a map[string]any) (any, error) {
			id, _ := a["id"].(string)
			if !selected[id] {
				return nil, fmt.Errorf("campaign row outside selected page")
			}
			return nil, operation(ctx, id)
		},
	}})
	return err
}

func campaignWorkflowCapabilities() []string {
	return []string{
		"campaignPromoteSeries", "campaignDrainWelcomes", "campaignPromoteSchedules", "campaignApplyWarmup", "campaignDrainableJobs", "campaignDrainJob", "campaignFlushReputation", "campaignProcessSelected",
	}
}
