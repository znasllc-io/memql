//go:build agent

package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/workbench"
)

func TestWorkbenchRecoveryContinuesSameGoalWithoutReplaying(t *testing.T) {
	var calls atomic.Int32
	d, store, _ := newWorkbenchHop(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		if dispatch.GetWorkbench().GetWorkspaceGeneration() != 1 || dispatch.GetRunId() != "goal-run" {
			t.Errorf("recovery lost goal/generation: %v", dispatch)
		}
		calls.Add(1)
		return okResult(dispatch.CallId), nil
	})
	store.binding.Expires = time.Now().Add(-time.Second)
	ctx := authorityCtx(t, sharedHopCaller)
	req := workbench.FleetRequest{RunID: "goal-run", Action: "exec", Args: map[string]any{"command": "must not replay"}}
	r, _ := d.DispatchFleetWorkbench(ctx, req)
	if r.ErrorCode != "workbench_state_lost" || calls.Load() != 0 {
		t.Fatalf("expired workspace executed: %+v", r)
	}
	recovery := workbench.FleetRequest{RunID: "goal-run", Action: "workspace_recover", Args: map[string]any{"generation": 0}}
	r, _ = d.DispatchFleetWorkbench(ctx, recovery)
	if !r.OK || calls.Load() != 0 || store.binding.Generation != 1 {
		t.Fatalf("recovery replayed or did not advance: %+v", r)
	}
	payload := r.Payload.(map[string]any)
	if payload["workspaceCreated"] != false || payload["state"] != "prepared" || payload["filesRestored"] != false || payload["operationReplayed"] != false {
		t.Fatalf("dishonest recovery receipt: %+v", payload)
	}
	r, _ = d.DispatchFleetWorkbench(ctx, recovery)
	if r.OK || store.binding.Generation != 1 {
		t.Fatal("stale recovery replaced the new workspace")
	}
	req.Action, req.Args = "fs_list", map[string]any{"path": "."}
	r, _ = d.DispatchFleetWorkbench(ctx, req)
	if !r.OK || calls.Load() != 1 {
		t.Fatalf("same goal could not continue: %+v", r)
	}
}

func TestWorkbenchRecoveryRequiresFreshAuthorityAndReportedLoss(t *testing.T) {
	for _, mode := range []string{"live-files", "paused-sharing", "cancelled-run", "foreign-owner", "wrong-generation"} {
		t.Run(mode, func(t *testing.T) {
			d, s, _ := newWorkbenchHop(t, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				t.Fatal("recovery must not dispatch a prior command")
				return nil, nil
			})
			s.binding.StateLost = mode != "live-files"
			owner := sharedHopCaller
			generation := 0
			switch mode {
			case "paused-sharing":
				s.paused = true
			case "cancelled-run":
				s.cancelled = true
			case "foreign-owner":
				owner = "another-person"
			case "wrong-generation":
				generation = 1
			}
			r, _ := d.DispatchFleetWorkbench(authorityCtx(t, owner), workbench.FleetRequest{RunID: "goal-run", Action: "workspace_recover", Args: map[string]any{"generation": generation}})
			if r.OK || s.binding.Generation != 0 {
				t.Fatalf("invalid recovery admitted: %+v", r)
			}
		})
	}
}

func TestQueuedWorkbenchCannotMoveIntoRecoveredGeneration(t *testing.T) {
	var calls atomic.Int32
	d, s, w := newWorkbenchHop(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		calls.Add(1)
		return okResult(dispatch.CallId), nil
	})
	if err := w.Acquire(context.Background(), WorkbenchCapability); err != nil {
		t.Fatal(err)
	}
	s.read = make(chan struct{}, 16)
	done := make(chan Result, 1)
	go func() {
		r, _, _ := d.remote.ForwardDispatch(authorityCtx(t, sharedHopCaller), nodeB, Request{Tool: WorkbenchTool, Action: "fs_list", Purpose: PurposeWorkbench, OwnerUserId: sharedHopCaller, RunId: "goal-run", workbenchGeneration: 0, Args: map[string]any{"path": "."}}, s.candidate.RegistrationId, WorkbenchCapability, 2*time.Second)
		done <- r
	}()
	select {
	case <-s.read:
	case <-time.After(time.Second):
		t.Fatal("receiver did not read authority")
	}
	s.mu.Lock()
	s.binding.Generation = 1
	s.mu.Unlock()
	w.Release(WorkbenchCapability)
	select {
	case r := <-done:
		if r.OK || calls.Load() != 0 {
			t.Fatalf("stale operation entered new filesystem: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued operation did not settle")
	}
}

func TestWorkbenchLostReceiptIsRecordedBeforeExplicitRecovery(t *testing.T) {
	var calls atomic.Int32
	d, s, _ := newWorkbenchHop(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		calls.Add(1)
		return &memqlv1.ToolResult{CallId: dispatch.CallId, Payload: &memqlv1.ToolResult_Failure{Failure: &memqlv1.Failure{ErrorCode: "workbench_state_lost", ErrorMessage: "interrupted; nothing replayed"}}}, nil
	})
	ctx := authorityCtx(t, sharedHopCaller)
	r, _ := d.DispatchFleetWorkbench(ctx, workbench.FleetRequest{RunID: "goal-run", Action: "exec", Args: map[string]any{"command": "once"}})
	if r.OK || calls.Load() != 1 || !s.binding.StateLost {
		t.Fatalf("lost receipt was hidden or replayed: %+v", r)
	}
	r, _ = d.DispatchFleetWorkbench(ctx, workbench.FleetRequest{RunID: "goal-run", Action: "workspace_recover", Args: map[string]any{"generation": 0}})
	if !r.OK || calls.Load() != 1 || s.binding.Generation != 1 {
		t.Fatalf("recovery replayed an uncertain effect: %+v", r)
	}
}
