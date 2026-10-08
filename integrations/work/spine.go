package work

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	workcore "github.com/znasllc-io/memql/component/work"
)

// CaptureSpine accepts only an installed template name. Intake uses it before
// opening any rows. The stored closure then crosses every planner hop without
// relying on a process cache or substituting the current installation's code.
func CaptureSpine(name string) (*workflowhost.Snapshot, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = workcore.DefaultSpine
	}
	ops := map[string]workflowhost.Operation{}
	for _, name := range workcore.SpineOperations() {
		ops[name] = func(context.Context, map[string]any) (any, error) {
			return nil, fmt.Errorf("Spine admission cannot execute operations")
		}
	}
	snapshot, err := workflowhost.Capture(name, workcore.SpineContract, nil, ops)
	if err != nil {
		return nil, err
	}
	// Spine entry points receive no positional data. Goal input is available
	// through spineContext; an unsatisfied entry must fail before opening rows.
	if err := snapshot.CheckArgs(nil); err != nil {
		return nil, fmt.Errorf("Spine %q entry arguments: %w", name, err)
	}
	return snapshot, nil
}
