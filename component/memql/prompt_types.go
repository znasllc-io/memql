package memql

import "strings"

// prompt_types.go defines the small in-package types the unified
// prompt loader consumes. These were defined inline in the legacy
// hand-rolled prompt_parser.go (and the field type in tool_parser.go)
// before #310 deleted those parser files; the types themselves stay
// alive because LoadUnifiedPrompts + prompt_converter.go still build
// + consume them on the langparser-backed prompt-load path.
//
//   - `promptDecl` carries one parsed `prompt NAME { ... }` block.
//     The langparser produces a public `ast.PromptDecl`; the
//     converter in prompt_converter.go translates that to this
//     in-package type so the JSON-schema compilation step on
//     `LoadUnifiedPrompts` doesn't need to read AST nodes directly.
//
//   - `toolField` is the field-list element used by both tool and
//     prompt schemas (prompts and tools share the same input-schema
//     shape: name + type + @required + @description + @enum +
//     @default + @autoInjected). With tool_parser.go gone the type
//     is now prompt-only at the use-site level, but keeping the
//     `toolField` name preserves grep continuity for anyone tracing
//     the legacy parser surface during the cleanup window.

// promptDecl is the unified loader's internal representation of a
// `prompt NAME { ... }` block.
type promptDecl struct {
	name            string
	description     string
	defaultProvider string
	templateSource  string // inline template from @template("""...""")
	templateFile    string // external template file from @templateFile("...")

	// Fields populates the prompt's input schema.
	fields []toolField
}

// toolField is one entry in a prompt's (or, historically, a tool's)
// input-schema field list. Matches the trailing-annotation syntax
// `name type @required @description("...") @enum("a", "b")
// @default("x") @autoInjected`.
type toolField struct {
	name         string
	typeName     string
	elementType  string // element type for `[]T` slice fields; "" for non-slice
	required     bool
	autoInjected bool // @autoInjected -- server stamp wins; LLM-supplied values dropped at dispatch
	description  string
	enumValues   []string
	defaultVal   string
}

// toInputSchema compiles a promptDecl's field list into the JSON-
// Schema map LoadUnifiedPrompts hands off to the schema compiler.
// Mirrors what the legacy parsePromptMemQL produced at the end of
// its parse: type-name lowering, default-string passthrough, array
// `items` fallback, enum coercion to `[]any`, required-field slice.
//
// Returns (nil, nil) when the decl has no fields (a valid prompt
// can take no input -- e.g. a static system-prompt template).
func (d *promptDecl) toInputSchema() (map[string]any, error) {
	if len(d.fields) == 0 {
		return nil, nil
	}

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}

	properties := make(map[string]any, len(d.fields))
	var required []string

	for _, f := range d.fields {
		prop := map[string]any{}

		switch strings.ToLower(f.typeName) {
		case "string":
			prop["type"] = "string"
		case "number", "float":
			prop["type"] = "number"
		case "integer", "int":
			prop["type"] = "integer"
		case "bool", "boolean":
			prop["type"] = "boolean"
		case "object":
			prop["type"] = "object"
		case "array":
			prop["type"] = "array"
			// OpenAI requires array schemas to specify "items" with a
			// "type" field. Default to generic object items when no
			// nested schema is defined (matches what parsePromptMemQL
			// emitted).
			prop["items"] = map[string]any{"type": "object"}
		default:
			prop["type"] = "string"
		}

		if f.description != "" {
			prop["description"] = f.description
		}
		if len(f.enumValues) > 0 {
			enumAny := make([]any, len(f.enumValues))
			for i, v := range f.enumValues {
				enumAny[i] = v
			}
			prop["enum"] = enumAny
		}
		if f.defaultVal != "" {
			prop["default"] = f.defaultVal
		}

		properties[f.name] = prop
		if f.required {
			required = append(required, f.name)
		}
	}

	schema["properties"] = properties
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, nil
}
