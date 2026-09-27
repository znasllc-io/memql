package steps

// step_inputs.go -- a person's inputs for one version of one step (epic
// memql#5414, design D20).
//
// A re-run or a branch may name ARGUMENTS to use instead of the step's own,
// by name; anything the person did not name keeps the value the step's own
// expression evaluates to. The executor puts the override on the targeted
// step's context and nowhere else (withRunContext), so reading it here --
// after the step's arguments were evaluated, before the call is rendered --
// is what makes the inputs apply to that version only and never leak into the
// next step.

import (
	"context"

	"github.com/znasllc-io/memql/core/common"
)

// withOverrideInputs returns args with the context's override inputs laid
// over them by name. It never mutates args: the evaluated map may be shared
// with the evaluator's own bindings. With no override, or one naming no
// inputs, args comes back as it went in.
func withOverrideInputs(ctx context.Context, args map[string]any) map[string]any {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.Override == nil || len(run.Override.Inputs) == 0 {
		return args
	}
	// Sized by the arguments alone: the map grows for the inputs, and a hint
	// that summed two lengths is an allocation size that can overflow.
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	for k, v := range run.Override.Inputs {
		out[k] = v
	}
	return out
}
