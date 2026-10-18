//go:build agent

package pipelinesteps

import (
	"context"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"testing"
)

func TestFleetRefusesHistoryBeforeTokenOrDispatch(t *testing.T) {
	req := testRequest()
	req.Step.CheckoutHistory = "full"
	run := stepRunFor(req, 600, pl.CodeRunCeiling)
	var fleet *Fleet
	result, err := fleet.RunStep(context.Background(), req, run)
	if err != nil || result.Failure == nil || result.Failure.Code != pl.CodeStepInvalid {
		t.Fatal("fleet silently ignored history contract")
	}
}
