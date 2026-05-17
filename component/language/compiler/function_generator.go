package compiler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// compileFunction converts a query or mutation FunctionDef to output format.
func (c *Compiler) compileFunction(def *parser.FunctionDef) (*FunctionOutput, error) {
	output := &FunctionOutput{
		Name: def.Name,
		Type: string(def.Type),
	}

	// Retain compiled definition metadata in-memory for tooling/debugging.
	definition := map[string]any{
		"$id":     fmt.Sprintf("fn:%s:args", def.Name),
		"$schema": "http://json-schema.org/draft-07/schema#",
		"title":   fmt.Sprintf("%s Arguments", def.Name),
		"type":    "object",
	}

	if def.Description != "" {
		definition["description"] = def.Description
	}

	// Build properties from args
	properties := make(map[string]any)
	required := []string{}

	for _, arg := range def.Args {
		prop := map[string]any{
			"type": "string", // default to string
		}
		if arg.Type != "" {
			prop["type"] = arg.Type
		}
		properties[arg.Name] = prop

		if arg.Required {
			required = append(required, arg.Name)
		}
	}

	definition["properties"] = properties
	if len(required) > 0 {
		definition["required"] = required
	}
	definition["additionalProperties"] = false

	output.Definition = definition

	// Generate the query string
	switch body := def.Body.(type) {
	case parser.ExpressionNode:
		output.Query = c.expressionToString(body)

	case *parser.MutationStmt:
		output.Query = c.mutationToString(body)

	case *parser.QueryStmt:
		output.Query = c.expressionToString(body.Expression)

	default:
		return nil, fmt.Errorf("unsupported function body type: %T", def.Body)
	}

	return output, nil
}

// ToJSON serializes the compile result to JSON bytes.
func (r *CompileResult) ToJSON(pretty bool) (map[string][]byte, error) {
	results := make(map[string][]byte)

	for _, automation := range r.Automations {
		var data []byte
		var err error
		if pretty {
			data, err = json.MarshalIndent(automation.JSON, "", "  ")
		} else {
			data, err = json.Marshal(automation.JSON)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to marshal automation %q: %w", automation.Name, err)
		}
		results[automation.Name+".json"] = data
	}

	for _, fn := range r.Functions {
		// Flat .memql file format: queryX.memql / mutationX.memql.
		prefix := "query"
		if strings.EqualFold(fn.Type, "mutation") {
			prefix = "mutation"
		}
		if fn.Name == "" {
			return nil, fmt.Errorf("function name is required for output")
		}
		fileName := prefix + strings.ToUpper(fn.Name[:1]) + fn.Name[1:] + ".memql"
		results[fileName] = []byte(fn.Query)
	}

	return results, nil
}
