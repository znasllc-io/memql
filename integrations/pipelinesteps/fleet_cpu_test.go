//go:build agent

package pipelinesteps

import (
	"context"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
	"os"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestFleetNativeCPURefusedBeforeDispatch(t *testing.T) {
	// Direct fleet dispatch must refuse either copy even without a dispatcher.
	for _, forwarded := range []bool{false, true} {
		req, run := exRequest(), testRun()
		run.Execution = pl.ExecutionNative
		if forwarded {
			run.CPUMilli = 4000
		} else {
			req.Step.CPUMilli = 4000
		}
		result, err := (*Fleet)(nil).RunStep(context.Background(), req, run)
		if err != nil || result.Failure == nil || result.Failure.Code != pl.CodeStepInvalid {
			t.Fatalf("fleet resource contract not refused before dispatch: %+v, %v", result, err)
		}
	}
}

func TestFleetContainerResourceLimitsReachWorker(t *testing.T) {
	run := testRun()
	run.Execution = pl.ExecutionContainer
	run.CPUMilli = 1250
	run.MemoryMiB = 1536
	args := fleetArgs(run, "")
	if args["cpuMilli"] != 1250 || args["memoryMiB"] != 1536 {
		t.Fatalf("lost declared resources: %v", args)
	}
}

func TestFleetEngineResourceCeilingsRefuseBeforeAnyEffect(t *testing.T) {
	for _, placement := range []string{pl.PlacementFleet, pl.PlacementPool} {
		for _, setting := range []string{"cpu", "memory", "default cpu", "default memory"} {
			for _, copyRun := range []bool{true, false} {
				t.Run(placement+"/"+setting+"/"+map[bool]string{true: "run", false: "request"}[copyRun], func(t *testing.T) {
					d := &fakeDispatcher{answer: func(context.Context, worker.Request) (worker.Result, error) {
						t.Fatal("over-limit step dispatched")
						return worker.Result{}, nil
					}}
					f, _, tokens, _ := newTestFleet(t, d)
					f.cfg.StepCPUMaxMilli = 2000
					f.cfg.StepMemoryMaxMiB = 4096
					req := exRequest()
					req.Step.Placement = placement
					req.Step.Execution = pl.ExecutionContainer
					req.Step.Needs = nil
					req.ComputePlacement = &pl.ComputePlacementPolicy{FleetWaitSeconds: 0, CloudFallback: true}
					run := fleetRun(req)
					cpu, mem := 0, 0
					switch setting {
					case "cpu":
						cpu = 2001
					case "memory":
						mem = 4097
					case "default cpu":
						f.cfg.StepCPUMaxMilli = 0
						cpu = defaultStepCPUMaxMilli + 1
					case "default memory":
						f.cfg.StepMemoryMaxMiB = 0
						mem = defaultStepMemoryMaxMiB + 1
					}
					if copyRun {
						run.CPUMilli, run.MemoryMiB = cpu, mem
					} else {
						req.Step.CPUMilli, req.Step.MemoryMiB = cpu, mem
					}
					result, err := f.RunStep(context.Background(), req, run)
					entries, readErr := os.ReadDir(f.tempDir)
					if err != nil || result.Failure == nil || result.Failure.Code != pl.CodeStepInvalid || len(tokens.asked) != 0 || readErr != nil || len(entries) != 0 {
						t.Fatalf("effectful refusal: %+v %v tokens=%v files=%v", result, err, tokens.asked, entries)
					}
				})
			}
		}
	}
}
