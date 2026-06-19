package steps

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// #1758 -- the action step type resolves to the ActionExecutor in the default
// registry, so an automation with an `action` step dispatches to replay.
func TestRegistry_ActionExecutorRegistered(t *testing.T) {
	r := NewRegistry()
	exec, ok := r.Get(automations.StepTypeAction)
	if !ok {
		t.Fatalf("StepTypeAction is not registered")
	}
	if _, ok := exec.(*ActionExecutor); !ok {
		t.Fatalf("StepTypeAction maps to %T, want *ActionExecutor", exec)
	}
}
