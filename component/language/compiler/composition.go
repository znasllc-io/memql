package compiler

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// CompositionError represents a file composition validation error.
type CompositionError struct {
	Message string
	Details []string
}

func (e *CompositionError) Error() string {
	if len(e.Details) == 0 {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Message, strings.Join(e.Details, ", "))
}

// FileComposition describes what a .memql file contains.
type FileComposition struct {
	Automations int
	Mutations   int
	Queries     int
	Total       int

	// Names of each type for error messages
	AutomationNames []string
	MutationNames   []string
	QueryNames      []string
}

// AnalyzeComposition examines a parsed file and returns its composition.
func AnalyzeComposition(file *parser.File) *FileComposition {
	comp := &FileComposition{}

	for _, def := range file.Definitions {
		switch d := def.(type) {
		case *parser.FunctionDef:
			switch d.Type {
			case parser.FunctionTypeAutomation:
				comp.Automations++
				comp.AutomationNames = append(comp.AutomationNames, d.Name)
			case parser.FunctionTypeMutation:
				comp.Mutations++
				comp.MutationNames = append(comp.MutationNames, d.Name)
			case parser.FunctionTypeQuery:
				comp.Queries++
				comp.QueryNames = append(comp.QueryNames, d.Name)
			default:
				// Default to query for untyped functions
				comp.Queries++
				comp.QueryNames = append(comp.QueryNames, d.Name)
			}
		}
	}

	comp.Total = comp.Automations + comp.Mutations + comp.Queries
	return comp
}

// ValidateFileComposition enforces CQS-based file composition rules:
//
// Rules:
//   - Max 1 automation per file (can have supporting queries)
//   - Max 1 mutation per file (can have supporting queries)
//   - Cannot mix automation and mutation in same file
//   - Unlimited queries per file
//   - Functions with an `args` parameter must declare a file-top
//     `args { ... }` block describing the input schema.
//
// Valid compositions:
//   - N queries (pure read module)
//   - 1 automation + N queries (automation with helpers)
//   - 1 mutation + N queries (mutation with validation helpers)
//
// Invalid compositions:
//   - 2+ automations
//   - 2+ mutations
//   - 1 automation + 1 mutation
//   - Function with `args` parameter but no `args { ... }` block
func ValidateFileComposition(file *parser.File) error {
	comp := AnalyzeComposition(file)

	// Rule 1: Max 1 automation per file
	if comp.Automations > 1 {
		return &CompositionError{
			Message: "only one automation definition allowed per file",
			Details: comp.AutomationNames,
		}
	}

	// Rule 2: Max 1 mutation per file
	if comp.Mutations > 1 {
		return &CompositionError{
			Message: "only one mutation definition allowed per file",
			Details: comp.MutationNames,
		}
	}

	// Rule 3: Cannot mix automation and mutation
	if comp.Automations > 0 && comp.Mutations > 0 {
		return &CompositionError{
			Message: "cannot mix automation and mutation in the same file",
			Details: append(
				prefixStrings("automation: ", comp.AutomationNames),
				prefixStrings("mutation: ", comp.MutationNames)...,
			),
		}
	}

	// Rule 4: Functions with an `args` parameter must declare a
	// file-top `args { ... }` block.
	if err := validateArgsSchemas(file); err != nil {
		return err
	}

	return nil
}

// validateArgsSchemas checks that functions with an `args`
// parameter have a corresponding file-top `args { ... }` block
// declaring the expected input schema.
func validateArgsSchemas(file *parser.File) error {
	for _, def := range file.Definitions {
		funcDef, ok := def.(*parser.FunctionDef)
		if !ok {
			continue
		}

		hasArgsParam := false
		for _, arg := range funcDef.Args {
			if arg.Name == "args" {
				hasArgsParam = true
				break
			}
		}

		if !hasArgsParam {
			continue
		}

		if funcDef.ArgsSchema == nil {
			return &CompositionError{
				Message: fmt.Sprintf("function %q has an `args` parameter but no file-top `args { ... }` block declares its input schema", funcDef.Name),
				Details: []string{
					"functions that read `args.X` in their body must declare the schema in a file-top `args { ... }` block above the `func (...)` declaration",
					"example: args { userId string @required; limit int }",
				},
			}
		}
	}

	return nil
}

// GetPrimaryType returns the primary (effect-owning) type in the file.
// Returns "query" if the file only contains queries.
func GetPrimaryType(file *parser.File) string {
	comp := AnalyzeComposition(file)

	if comp.Automations > 0 {
		return "automation"
	}
	if comp.Mutations > 0 {
		return "mutation"
	}
	return "query"
}

// GetPrimaryName returns the name of the primary definition.
// For query-only files, returns the first query name.
func GetPrimaryName(file *parser.File) string {
	comp := AnalyzeComposition(file)

	if len(comp.AutomationNames) > 0 {
		return comp.AutomationNames[0]
	}
	if len(comp.MutationNames) > 0 {
		return comp.MutationNames[0]
	}
	if len(comp.QueryNames) > 0 {
		return comp.QueryNames[0]
	}
	return ""
}

// CompositionSummary returns a human-readable summary of file composition.
func CompositionSummary(file *parser.File) string {
	comp := AnalyzeComposition(file)

	parts := []string{}
	if comp.Automations > 0 {
		parts = append(parts, fmt.Sprintf("%d automation", comp.Automations))
	}
	if comp.Mutations > 0 {
		parts = append(parts, fmt.Sprintf("%d mutation", comp.Mutations))
	}
	if comp.Queries > 0 {
		if comp.Queries == 1 {
			parts = append(parts, "1 query")
		} else {
			parts = append(parts, fmt.Sprintf("%d queries", comp.Queries))
		}
	}

	if len(parts) == 0 {
		return "empty file"
	}
	return strings.Join(parts, ", ")
}

// prefixStrings adds a prefix to each string in the slice.
func prefixStrings(prefix string, strs []string) []string {
	result := make([]string, len(strs))
	for i, s := range strs {
		result[i] = prefix + s
	}
	return result
}
