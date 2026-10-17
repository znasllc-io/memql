package work

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func (i *Integration) handleOpenModelCatalogResearch(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireClusterOwner(ctx); err != nil {
		return nil, err
	}
	if rc, ok := common.RunFromContext(ctx); ok && rc.IsRun() {
		return nil, fmt.Errorf("model runs cannot start catalog research")
	}
	rows, err := i.store().query(memql.ContextWithFreshRead(ctx), "modelCatalogResearchSettings()")
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || rows[0]["enabled"] != true {
		return nil, fmt.Errorf("Catalog research is off")
	}
	owner := strings.TrimSpace(rowString(rows[0], "ownerUserId"))
	if owner == "" {
		return nil, fmt.Errorf("Choose a research owner before enabling catalog research")
	}
	choice := common.RouteChoice{Source: "fleet:strongest", Level: "strong", By: owner, NoPaidProviders: true}
	ctx = memql.ContextWithGoalRouteChoice(ctx, choice)
	goal, run, err := i.OpenDirectGoal(ctx, DirectGoal{
		OwnerUserId: owner, Statement: "Review the local model catalog against current primary sources", AutomationName: "researchModelCatalog",
		Input: map[string]any{}, RequestedVia: "api", TriggeredBy: "catalog.weekly",
		Ceilings: map[string]any{"tokenBudget": 120000, "wallClockMs": 5400000, "maxModelCalls": 24, "maxRetries": 1, "maxEvents": 200},
	})
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{"goalId": goal, "runId": run}), nil
}
