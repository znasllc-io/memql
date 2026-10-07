//go:build agent

package pipelinesteps

import (
	"context"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestFleetCPURefusedBeforeDispatch(t *testing.T) {
	// Direct fleet dispatch must refuse either copy even without a dispatcher.
	for _, forwarded := range []bool{false, true} {
		req, run := exRequest(), testRun()
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
