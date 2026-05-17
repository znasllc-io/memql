// Package compiler transpiles MemQL AST to various output formats.
// The primary use case is converting .memql function/automation definitions
// to .json files that the automation scheduler can consume.
package compiler

import (
	"github.com/znasllc-io/memql/component/language/parser"
)

type (
	// Compiler transpiles MemQL AST to target formats.
	Compiler struct {
		config Config
	}

	// Config holds compiler configuration.
	Config struct {
		// PrettyPrint enables formatted JSON output
		PrettyPrint bool
		// Indent specifies the indentation string (default: "  ")
		Indent string
		// IncludeComments preserves comments from source in output (where possible)
		IncludeComments bool
		// StrictWarnings treats linter warnings as compile errors.
		StrictWarnings bool
	}
)

// DefaultConfig returns the default compiler configuration.
func DefaultConfig() Config {
	return Config{
		PrettyPrint: true,
		Indent:      "  ",
	}
}

// New creates a new compiler with the given configuration.
func New(cfg Config) *Compiler {
	return &Compiler{config: cfg}
}

// NewDefault creates a new compiler with default configuration.
func NewDefault() *Compiler {
	return New(DefaultConfig())
}

// CompileFile compiles a parsed File to output artifacts.
// It first validates file composition according to CQS rules:
//   - Max 1 automation per file (can have supporting queries)
//   - Max 1 mutation per file (can have supporting queries)
//   - Cannot mix automation and mutation in same file
//   - Unlimited queries per file
func (c *Compiler) CompileFile(file *parser.File) (*CompileResult, error) {
	// Validate file composition (CQS rules)
	if err := ValidateFileComposition(file); err != nil {
		return nil, err
	}
	if err := ValidateCQS(collectFunctionDefs(file)); err != nil {
		return nil, err
	}

	warnings := LintFile(file)
	if c.config.StrictWarnings && len(warnings) > 0 {
		return nil, &LintError{Warnings: warnings}
	}

	result := &CompileResult{
		Automations: []AutomationOutput{},
		Functions:   []FunctionOutput{},
		Warnings:    warnings,
	}

	for _, def := range file.Definitions {
		switch d := def.(type) {
		case *parser.FunctionDef:
			switch d.Type {
			case parser.FunctionTypeAutomation:
				automation, err := c.compileAutomation(d)
				if err != nil {
					return nil, err
				}
				result.Automations = append(result.Automations, *automation)

			case parser.FunctionTypeQuery, parser.FunctionTypeMutation, parser.FunctionTypeSpec, parser.FunctionTypeTool, parser.FunctionTypeBuiltin:
				fn, err := c.compileFunction(d)
				if err != nil {
					return nil, err
				}
				result.Functions = append(result.Functions, *fn)
			}
		}
	}

	return result, nil
}

// CompileResult contains the outputs from compilation.
type CompileResult struct {
	Automations []AutomationOutput
	Functions   []FunctionOutput
	Warnings    []Warning
}

func collectFunctionDefs(file *parser.File) []*parser.FunctionDef {
	if file == nil {
		return nil
	}
	out := make([]*parser.FunctionDef, 0, len(file.Definitions))
	for _, def := range file.Definitions {
		if fn, ok := def.(*parser.FunctionDef); ok {
			out = append(out, fn)
		}
	}
	return out
}

// AutomationOutput represents a compiled automation ready for JSON serialization.
type AutomationOutput struct {
	Name        string
	Description string
	JSON        map[string]any
}

// FunctionOutput represents a compiled function.
type FunctionOutput struct {
	Name       string
	Type       string
	Definition map[string]any
	Query      string
}
