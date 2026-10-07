package workflowhost

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

type preparedLogic struct {
	body     *automations.Automation
	validate func(map[string]any) error
}

var installedLogics = sync.OnceValues(func() (map[string]func() (*memql.Function, error), error) {
	result := map[string]func() (*memql.Function, error){}
	for _, raw := range baseloader.ReadAll(slog.New(slog.DiscardHandler)) {
		for _, slice := range memql.ExtractFunctionSlices(raw.Content) {
			if slice.Kind != parser.FunctionTypeLogic {
				continue
			}
			if result[slice.Name] != nil {
				return nil, fmt.Errorf("duplicate workflow logic %q", slice.Name)
			}
			// Compile only the reachable pure definitions. Unrelated graph-reading
			// logics require a concept registry that this restricted host lacks.
			result[slice.Name] = sync.OnceValues(func() (*memql.Function, error) {
				return memql.BuildFunctionConstruct(slice.Source, slice.Name, "unified:"+raw.Path, nil)
			})
		}
	}
	return result, nil
})

func loadLogic(name string) (*memql.Function, error) {
	all, err := installedLogics()
	if err != nil {
		return nil, err
	}
	if build := all[name]; build != nil {
		return build()
	}
	return nil, fmt.Errorf("workflow logic %q is not installed", name)
}

// A scoped logic computes over supplied values. It cannot acquire even a
// read-only engine port, invoke a callback, or escape through a child automation.
// Gated functions are refused rather than silently bypassing their entry gates.
func (h *host) prepareLogic(name string, visiting map[string]bool) error {
	key := "logic:" + name
	if visiting[key] {
		return fmt.Errorf("workflow logic recursion at %q", name)
	}
	if h.logics[name] != nil {
		return nil
	}
	fn, err := h.opts.LoadLogic(name)
	if err != nil {
		return err
	}
	if fn == nil || fn.Name != name || fn.FunctionKind != "logic" || !fn.Enabled || fn.LogicBody == nil || fn.ServerOnly || fn.RequiresRank != "" || fn.RequiresCapability != (memql.CapabilityRequirement{}) {
		return fmt.Errorf("workflow logic %q is not an enabled ungated value computation", name)
	}
	a, err := automations.NewLogicRunner(nil, nil, h.opts.Logger).PrepareLogicBody(fn.Name, fn.LogicBody)
	if err != nil {
		return err
	}
	visiting[key] = true
	var walk func([]*automations.Step) error
	walk = func(body []*automations.Step) error {
		for _, step := range body {
			switch step.Type {
			case automations.StepTypeExpression, automations.StepTypeReturn:
			case automations.StepTypeFunction:
				if step.Function == nil || step.Function.Kind != "logic" {
					return fmt.Errorf("workflow logic %q may only call other pure logics", name)
				}
				if err := h.prepareLogic(step.Function.Name, visiting); err != nil {
					return err
				}
			case automations.StepTypeBlock:
				if step.Block == nil {
					return fmt.Errorf("workflow logic %q has no block body", name)
				}
				if err := walk(step.Block.Steps); err != nil {
					return err
				}
			case automations.StepTypeForEach:
				if step.ForEach == nil {
					return fmt.Errorf("workflow logic %q has no loop body", name)
				}
				if err := walk(step.ForEach.Do); err != nil {
					return err
				}
			default:
				return fmt.Errorf("workflow logic %q step %q is outside its pure contract", name, step.Type)
			}
		}
		return nil
	}
	if err := walk(a.Steps); err != nil {
		return err
	}
	delete(visiting, key)
	h.logics[name] = &preparedLogic{body: a, validate: fn.SnapshotArgumentValidator()}
	return nil
}
