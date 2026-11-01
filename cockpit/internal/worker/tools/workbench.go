package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/cockpit/internal/worker/workbench"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"time"
)

var isolatedWorkbenches = workbench.New(workbench.Docker{}, "")

func (d *Dispatcher) WorkbenchReady(ctx context.Context) bool {
	if d == nil || d.pipelineHome == "" || !d.policy.WorkbenchesAllowed() {
		return false
	}
	_, err := (workbench.Docker{}).Ready(ctx)
	return err == nil
}
func (d *Dispatcher) RunWorkbenchLifecycle(ctx context.Context) func() {
	lifecycle, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := isolatedWorkbenches.Reconcile(lifecycle); err != nil && lifecycle.Err() == nil {
			d.logger.Warn("Fleet workbench cleanup needs attention", "error", err)
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-lifecycle.Done():
				return
			case <-ticker.C:
				isolatedWorkbenches.Sweep()
				cleanup, stop := context.WithTimeout(lifecycle, 20*time.Second)
				if err := isolatedWorkbenches.Reconcile(cleanup); err != nil && lifecycle.Err() == nil {
					d.logger.Debug("Fleet workbench cleanup still pending", "error", err)
				}
				stop()
			}
		}
	}()
	return func() {
		cancel()
		<-done
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			if isolatedWorkbenches.CloseHome(d.pipelineHome) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		d.logger.Warn("Fleet workbench cleanup is pending; its reservations remain fenced")
	}
}
func (d *Dispatcher) dispatchWorkbench(ctx context.Context, dispatch *memqlv1.ToolDispatch, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	refuse := func(code, message string) (*memqlv1.Success, *memqlv1.Failure) {
		return nil, &memqlv1.Failure{ErrorCode: code, ErrorMessage: message}
	}
	c := dispatch.GetWorkbench()
	now := time.Now()
	if dispatch.GetTool() != "workerWorkbench" || c == nil || dispatch.GetComputerPermission() != nil || c.GetRunId() != dispatch.GetRunId() || c.GetExpiresUnix() < now.Unix() || c.GetExpiresUnix() > now.Add(time.Minute).Unix()+5 || d.pipelineHome == "" {
		return refuse("workbench_constraint_refused", "This request does not carry a current isolated-workbench admission.")
	}
	if !d.policy.WorkbenchesAllowed() {
		return refuse("workbench_not_enabled", "Enable Fleet workbenches in Cockpit setup on this machine.")
	}
	deadline := time.Unix(c.GetWorkspaceExpiresUnix(), 0)
	if !now.Before(deadline) || deadline.After(now.Add(workbench.Lifetime+time.Second)) {
		return refuse("workbench_constraint_refused", "This goal's workbench lease has ended.")
	}
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	result, err := isolatedWorkbenches.Execute(runCtx, workbench.Identity{Home: d.pipelineHome, Owner: c.GetOwnerUserId(), Caller: c.GetCallerUserId(), Registration: c.GetRegistrationId(), Machine: c.GetMachineId(), Run: c.GetRunId(), ExpiresAt: deadline, Generation: c.GetWorkspaceGeneration()}, workbench.Request{Action: dispatch.GetAction(), Args: args})
	if errors.Is(err, workbench.ErrBusy) {
		return refuse("workbench_capacity_busy", "This machine is at workbench capacity; no operation started.")
	}
	if errors.Is(err, workbench.ErrStateLost) {
		return refuse("workbench_state_lost", err.Error())
	}
	if err != nil {
		return refuse("workbench_unavailable", err.Error())
	}
	if !result.OK {
		return refuse(result.ErrorCode, result.ErrorMessage)
	}
	body, err := json.Marshal(result.Payload)
	if err != nil {
		return refuse("workbench_invalid_result", fmt.Sprint(err))
	}
	return &memqlv1.Success{ResultJson: body}, nil
}
