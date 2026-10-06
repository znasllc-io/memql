//go:build agent

package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// TestTheAgentNodeRegistersThePipelinesExecutor: on an agent node the wiring
// registers the substrate's executor -- even one missing both halves, which
// then names what it lacks for each kind of step -- and on any other node
// type it registers nothing, because the driver calls Execute on agent nodes
// only.
func TestTheAgentNodeRegistersThePipelinesExecutor(t *testing.T) {
	previous := pipelines.RegisterExecutor(nil)
	t.Cleanup(func() { pipelines.RegisterExecutor(previous) })
	a := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	a.wirePipelinesExecutor(&node.Identity{ID: "workbench-a", Type: node.NodeTypeWorkbench})
	if exec := pipelines.CurrentExecutor(); exec != nil {
		t.Fatalf("a workbench node registered an executor (%T); the driver runs on agent nodes only", exec)
	}

	a.wirePipelinesExecutor(&node.Identity{ID: "agent-a", Type: node.NodeTypeAgent})
	exec, ok := pipelines.CurrentExecutor().(*pipelinesteps.Executor)
	if !ok {
		t.Fatalf("the agent node registered %T, want the substrate's *pipelinesteps.Executor", pipelines.CurrentExecutor())
	}

	step := pipelines.StepRequest{
		RunID: "run-1", WorkRunID: "work-1", StepKey: "tests.unit", Attempt: 1, OwnerUserID: "user-1",
		RunStartedAt: time.Now().UTC().Format(time.RFC3339),
		Repository:   pipelines.Repository{Owner: "acme", Name: "widget"},
		Compute:      pipelines.ComputeClusterAndFleet,
		Step:         pipelines.Step{Key: "tests.unit", Kind: pipelines.StepCommand, Run: "go test ./...", Image: "toolchain:1", Execution: pipelines.ExecutionContainer},
	}
	for _, needs := range [][]string{nil, {"docker"}} {
		step.Step.Needs = needs
		if len(needs) > 0 {
			step.Step.Execution = pipelines.ExecutionNative
			step.Step.Placement = pipelines.PlacementFleet
			step.Step.Platform = "linux/amd64"
			step.Step.Image = ""
		}
		res, err := exec.Execute(context.Background(), step)
		if err != nil {
			t.Fatalf("needs %v: Execute: %v", needs, err)
		}
		if res.Status != pipelines.OutcomeFailed || res.Failure == nil || res.Failure.Code != pipelines.CodeRunnerUnavailable {
			t.Errorf("needs %v: %+v (failure %+v), want %s naming the missing half", needs, res, res.Failure, pipelines.CodeRunnerUnavailable)
		}
	}
}
