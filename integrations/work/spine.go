package work

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	workcore "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// CaptureSpine accepts only an installed template name. Intake uses it before
// opening any rows. The stored closure then crosses every planner hop without
// relying on a process cache or substituting the current installation's code.
func CaptureSpine(name string) (*workflowhost.Snapshot, error) {
	return captureSpine(name, workflowhost.InstalledSource)
}

func captureSpine(name string, source workflowhost.SourceLoader) (*workflowhost.Snapshot, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = workcore.DefaultSpine
	}
	ops := map[string]workflowhost.Operation{}
	for _, name := range append(append(workcore.SpineOperations(), workcore.SpineDraftOperations()...), workcore.SpineRemedyOperations()...) {
		ops[name] = func(context.Context, map[string]any) (any, error) {
			return nil, fmt.Errorf("Spine admission cannot execute operations")
		}
	}
	for _, name := range []string{"agentRecoveryContext", "integration.agents.spineRetryHost", "integration.agents.spineObserveComputer", "integration.agents.spineRequestScope"} {
		ops[name] = func(context.Context, map[string]any) (any, error) {
			return nil, fmt.Errorf("Spine admission cannot execute operations")
		}
	}
	phases := map[string]string{}
	for _, phase := range []string{"workSpineDraftProgram", "agentWorkbenchRecovery", "workSpineRecovery", "workSpineReplan", "workSpineRepair"} {
		phases[phase] = phase
	}
	var entries []string
	// An optional ordinary, effect-free template configures hook reuse. Capture
	// and run it before opening any rows, then freeze the selected closure.
	descriptor := name + "Phases"
	if _, err := source("automation", descriptor); err == nil {
		config, err := workflowhost.Capture(descriptor, workcore.SpineContract, source, nil)
		if err != nil {
			return nil, fmt.Errorf("Spine phases: %w", err)
		}
		value, err := workflowhost.RunSnapshot(context.Background(), config, workcore.SpineContract, nil, workflowhost.Options{})
		if err != nil {
			return nil, fmt.Errorf("Spine phases: %w", err)
		}
		selected, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Spine phases must return an object")
		}
		for phase, value := range selected {
			if _, known := phases[phase]; !known {
				return nil, fmt.Errorf("unknown Spine phase %q", phase)
			}
			target, ok := value.(string)
			if !ok || strings.TrimSpace(target) == "" {
				return nil, fmt.Errorf("Spine phase %q requires a template name", phase)
			}
			phases[phase] = target
		}
		entries = append(entries, descriptor)
	} else if !errors.Is(err, workflowhost.ErrSourceNotFound) {
		return nil, err
	}
	snapshot, err := workflowhost.CapturePhases(name, workcore.SpineContract, phases, entries, source, ops)
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

// goalSpine pins direct execution too. Delegation by the same owner's run
// retains its admitted recipes; a separate owner's goal gets the default.
func goalSpine(ctx context.Context, owner string) (map[string]any, error) {
	if run, ok := common.RunFromContext(ctx); ok && run.OwnerUserId != "" && memql.BareShortId(run.OwnerUserId) == memql.BareShortId(owner) && run.Spine != nil {
		snapshot, err := workflowhost.SnapshotFromMap(run.Spine)
		if err != nil {
			return nil, err
		}
		if snapshot.Contract != workcore.SpineContract {
			return nil, fmt.Errorf("incompatible inherited Spine contract")
		}
		return snapshot.Map(), nil
	}
	snapshot, err := CaptureSpine("")
	if err != nil {
		return nil, err
	}
	return snapshot.Map(), nil
}
