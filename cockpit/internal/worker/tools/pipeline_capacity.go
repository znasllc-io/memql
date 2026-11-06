package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// pipelineSlotUse is what the step holding a slot asked for: the step it is,
// and what another step's admission charges against the machine for it.
type pipelineSlotUse struct {
	// step names the dispatch's work run and step key; attempts of one step
	// share it, and two never hold slots at once. Empty when the dispatch named
	// neither, and such a step runs only alone.
	step string
	// cpuMilli and memoryMiB are the command's admitted limits plus every
	// service's, as checkPipelineHeadroom charges its own step.
	cpuMilli, memoryMiB int
	// docker is set for a step that runs in, or reaches, the Docker daemon.
	docker bool
}

// pipelineStepKey carries the dispatch's run and step into runPipelineStep.
type pipelineStepKey struct{}

// withPipelineStep records which step a dispatch is: the work run and step key
// the engine names on every pipeline dispatch
// (integrations/pipelinesteps/fleet.go, dispatchStep). Every attempt of a step
// carries the same pair. A dispatch missing either is left unidentified.
func withPipelineStep(ctx context.Context, runID, stepID string) context.Context {
	runID, stepID = strings.TrimSpace(runID), strings.TrimSpace(stepID)
	if runID == "" || stepID == "" {
		return ctx
	}
	return context.WithValue(ctx, pipelineStepKey{}, runID+"\x00"+stepID)
}

func pipelineStepOf(ctx context.Context) string {
	step, _ := ctx.Value(pipelineStepKey{}).(string)
	return step
}

// pipelineStepDemand is what admission charges for req: the command's admitted
// limits plus every service's fixed caps.
func pipelineStepDemand(req pipelineStepRequest) (cpuMilli, memoryMiB int) {
	return req.cpuMilli + len(req.services)*pipelineServiceCPUMilli, req.memoryMiB + len(req.services)*pipelineServiceMemoryMiB
}

func pipelineSlotUseOf(ctx context.Context, req pipelineStepRequest) pipelineSlotUse {
	cpu, memory := pipelineStepDemand(req)
	return pipelineSlotUse{
		step:      pipelineStepOf(ctx),
		cpuMilli:  cpu,
		memoryMiB: memory,
		docker:    req.execution == "container" || slices.Contains(req.needs, "docker"),
	}
}

// reconcileInterruptedPipelines reconciles every interrupted attempt on this
// machine before own's step starts: own's slot when its record outlived an
// earlier step, and each slot no running step holds whose record did. A slot a
// running step holds is never read or touched, so this runs beside steps in
// other slots. Any attempt left unreconciled keeps the machine from admitting
// the step, as the one record always did; errPipelineBusy means another
// reconciliation is under way.
func reconcileInterruptedPipelines(ctx context.Context, own *pipelineReservation) error {
	claims, err := claimInterruptedPipelineSlots(own)
	if err != nil {
		return err
	}
	var failures []error
	if own.dirty {
		if err := reconcilePipelineReservation(ctx, own); err != nil {
			failures = append(failures, err)
		}
	}
	for _, claim := range claims {
		if err := reconcilePipelineReservation(ctx, claim); err != nil {
			failures = append(failures, err)
		}
		claim.close()
	}
	return errors.Join(failures...)
}
