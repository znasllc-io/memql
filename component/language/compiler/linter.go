package compiler

import (
	"fmt"
	"strings"

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

// LintFile returns compile-time warnings for naming and deprecated syntax usage.
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
		warnings = append(warnings, lintFunctionNaming(fn)...)
		warnings = append(warnings, lintAutomationSteps(fn)...)
	}
	return warnings
}

func lintFunctionNaming(fn *parser.FunctionDef) []Warning {
	if fn == nil {
		return nil
	}

	var (
		expectedPrefix string
		ruleName       string
		kindName       string
	)

	switch fn.Type {
	case parser.FunctionTypeQuery:
		expectedPrefix = "query"
		ruleName = "naming.query-prefix"
		kindName = "Query"
	case parser.FunctionTypeMutation:
		expectedPrefix = "mutation"
		ruleName = "naming.mutation-prefix"
		kindName = "Mutation"
	case parser.FunctionTypeSpec:
		expectedPrefix = "spec"
		ruleName = "naming.spec-prefix"
		kindName = "Spec"
	default:
		return nil
	}

	if strings.HasPrefix(fn.Name, expectedPrefix) {
		return nil
	}
	if fn.Name == "" {
		return nil
	}

	suggested := expectedPrefix + strings.ToUpper(fn.Name[:1]) + fn.Name[1:]
	return []Warning{{
		Level:      "info",
		Rule:       ruleName,
		Message:    fmt.Sprintf("%s function %q should use %q prefix", kindName, fn.Name, expectedPrefix),
		Suggestion: fmt.Sprintf("Rename %q to %q", fn.Name, suggested),
	}}
}

