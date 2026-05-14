package compiler

import (
	"fmt"

	"github.com/visionarys-io/memql/component/language/parser"
)

// lintAutomationSteps contains automation-specific lints so they stay isolated
// from generic function lint rules.
func lintAutomationSteps(fn *parser.FunctionDef) []Warning {
	if fn == nil || fn.Type != parser.FunctionTypeAutomation {
		return nil
	}

	automation, ok := fn.Body.(*parser.AutomationDef)
	if !ok {
		return nil
	}

	var warnings []Warning
	var visit func(steps []parser.StepDef)
	visit = func(steps []parser.StepDef) {
		for _, step := range steps {
			switch step.Type {
			case parser.StepTypeQuery, parser.StepTypeMutation:
				warnings = append(warnings, Warning{
					Level:      "warning",
					Rule:       "deprecation.inline-block-step",
					Message:    fmt.Sprintf("Automation step %q uses deprecated inline %q block syntax", step.ID, step.Type),
					Suggestion: "Extract a named function and invoke it via function-call step syntax",
				})
			case parser.StepTypeForEach:
				if cfg, ok := step.Config.(*parser.ForEachStepConfig); ok {
					visit(cfg.Do)
				}
			case parser.StepTypeParallel:
				if cfg, ok := step.Config.(*parser.ParallelStepConfig); ok {
					visit(cfg.Branches)
				}
			case parser.StepTypeSwitch:
				if cfg, ok := step.Config.(*parser.SwitchStepConfig); ok {
					for _, sc := range cfg.Cases {
						visit(sc.Steps)
					}
					if cfg.Default != nil {
						visit(cfg.Default.Steps)
					}
				}
			}
		}
	}
	visit(automation.Steps)
	return warnings
}
