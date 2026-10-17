package work

import (
	"context"
	"errors"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func (i *Integration) CheckExecutionDestination(ctx context.Context, runID, target string) error {
	return memql.CheckExecutionDestination(ctx, i.engine, runID, target)
}

// A computer-selected goal needs the person's recorded decision before any
// operation moves to the hosted workspace. This grants no computer access.
func (i *Integration) handleRequestWorkbenchUse(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.GoalId == "" || rc.OwnerUserId == "" {
		return nil, fmt.Errorf("Workbench permission requires an owned goal")
	}
	reason := argString(args, "reason")
	if reason == "" || len(reason) > 2000 {
		return nil, fmt.Errorf("explain why this work needs the Workbench")
	}
	err := memql.CheckExecutionDestination(ctx, i.engine, rc.RunId, "workbench")
	if err == nil {
		return i.resultNode(map[string]any{"status": "approved", "destination": "workbench", "runId": rc.RunId}), nil
	}
	var required *memql.ExecutionDestinationRequired
	if !errors.As(err, &required) {
		return nil, err
	}
	subject := map[string]any{"executionDestination": required.Subject(), "summary": "Use the Workbench for this Computer goal? " + reason}
	if binder, ok := i.engine.(interface {
		BindWorkScopeCheckpoint(context.Context, map[string]any, map[string]any) (map[string]any, error)
	}); ok {
		subject, err = binder.BindWorkScopeCheckpoint(ctx, args, subject)
		if err != nil {
			return nil, err
		}
	}
	approval, err := i.AskComputerScope(ctx, rc.OwnerUserId, rc.RunId, subject)
	if err != nil {
		return nil, err
	}
	row, err := one(i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workApprovalForOwner", map[string]any{"approvalId": approval})))
	if err != nil {
		return nil, err
	}
	if rowString(row, "decision") == "rejected" {
		return nil, fmt.Errorf("Workbench use was declined")
	}
	status := "awaiting_user"
	if err := memql.CheckExecutionDestination(ctx, i.engine, rc.RunId, "workbench"); err == nil {
		status = "approved"
	}
	return i.resultNode(map[string]any{"status": status, "destination": "workbench", "runId": rc.RunId, "approvalId": approval}), nil
}
