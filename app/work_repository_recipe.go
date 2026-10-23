package app

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

// Repository goals keep normal Work journals and claims, but their source and
// callable operations come from the native request's exact approved closure.
// Recovery on another replica must pass this gate before the first step too.
func loadRepositoryWorkTemplate(ctx context.Context, engine *memql.MemQLEngine, journal *automations.RunJournal) (*workTemplate, error) {
	if engine == nil || journal.TemplateConstructId != "" || journal.ForkedFromRunId != "" {
		return nil, fmt.Errorf("repository work cannot substitute an authored or forked recipe")
	}
	provider, ok := engine.IntegrationByName("repositoryauthority").(*repositoryauthority.Integration)
	if !ok || provider == nil {
		return nil, fmt.Errorf("repository execution authority is unavailable")
	}
	input, _ := journal.Input.(map[string]any)
	requestID, _ := input["requestId"].(string)
	snapshot, err := provider.ExecutionRecipe(ctx, requestID, journal.AutomationName)
	if err != nil {
		return nil, err
	}
	operations := map[string]workflowhost.Operation{}
	for _, name := range []string{"repositoryChangeContext", "ai", "repositoryChangePropose"} {
		operations[name] = func(c context.Context, args map[string]any) (any, error) {
			// Recheck before every bounded call, including model calls. Nothing
			// in the saved source replaces a current approval or live run.
			if _, err := provider.ExecutionRecipe(c, requestID, journal.AutomationName); err != nil {
				return nil, err
			}
			call, err := parser.RenderCall(name, args)
			if err != nil {
				return nil, err
			}
			return engine.Execute(c, call)
		}
	}
	prepared, err := workflowhost.PrepareSnapshot(snapshot, snapshot.Contract, workflowhost.Options{AmbientEngine: engine, Logger: engine.Logger, Operations: operations})
	if err != nil {
		return nil, err
	}
	auto, err := prepared.Automation(journal.AutomationName)
	if err != nil {
		return nil, err
	}
	return &workTemplate{automation: auto, prepared: prepared}, nil
}
