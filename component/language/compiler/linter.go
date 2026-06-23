package compiler

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Warning is a non-fatal diagnostic from compile-time linting.
type Warning struct {
	Level      string `json:"level"`
	Rule       string `json:"rule"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

// LintError turns warnings into a hard failure in strict mode.
type LintError struct {
	Warnings []Warning
}

func (e *LintError) Error() string {
	if len(e.Warnings) == 0 {
		return "lint: warnings treated as errors"
	}
	w := e.Warnings[0]
	return fmt.Sprintf("lint (%s): %s", w.Rule, w.Message)
}

// LintFile returns compile-time warnings for deprecated syntax usage.
//
// The naming-prefix lint (query* / mutation* / spec* required names)
// was retired in the DSL grammar redesign (epic #2031, C2/#2042):
// references resolve structurally by slot keyword + enclosing concept,
// so a construct's NAME is free. The dependency-tree validator
// (C3/#2043, component/memql.ValidateDependencyTree) replaces the
// prefix convention with a structural correctness check at load time.
func LintFile(file *parser.File) []Warning {
	if file == nil {
		return nil
	}

	var warnings []Warning
	for _, def := range file.Definitions {
		fn, ok := def.(*parser.FunctionDef)
		if !ok {
			continue
		}
		warnings = append(warnings, lintAutomationSteps(fn)...)
	}
	return warnings
}
