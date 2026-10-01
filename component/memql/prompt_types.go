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

// promptSchemaType is the JSON-Schema type a prompt field's declared type
// publishes as: the one mapping toInputSchema and the @default check share,
// so a default is held to the type the model is told. A type outside the
// list publishes as a string, as it always has.
func promptSchemaType(typeName string) string {
	switch strings.ToLower(typeName) {
	case "number", "float":
		return "number"
	case "integer", "int":
		return "integer"
	case "bool", "boolean":
		return "boolean"
	case "object":
		return "object"
	case "array":
		return "array"
	}
	return "string"
}

// promptArrayItems is the items schema for a `[]elem` field. `array` written
// without the shorthand names no element and keeps generic object items.
func promptArrayItems(elem string) map[string]any {
	switch {
	case elem == "":
		return map[string]any{"type": "object"}
	case strings.HasPrefix(elem, "[]"):
		return map[string]any{"type": "array", "items": promptArrayItems(strings.TrimPrefix(elem, "[]"))}
	}
	return map[string]any{"type": promptSchemaType(elem)}
}

// promptDecl is the unified loader's internal representation of a
// `prompt NAME { ... }` block.
type promptDecl struct {
	docComment      string
	name            string
	description     string
	level           string // @level -- how much intelligence the call needs
	defaultProvider string
	templateSource  string // inline template from @template("""...""")
	templateFile    string // external template file from @templateFile("...")
	disabled        bool   // @disabled -- the loader skips registration (#2606)

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
	// defaultSet reports that @default was written (defaultVal may be
	// empty), defaultNumber that it was written as an unquoted number, and
	// defaultValue is the literal it reads as in the field's type -- what
	// the schema publishes (memql#5430). A prompt's converter sets all
	// three; the check that fills defaultValue is promptDefaultValue.
	defaultSet    bool
	defaultNumber bool
	defaultValue  any
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

		prop["type"] = promptSchemaType(f.typeName)
		if prop["type"] == "array" {
			// OpenAI requires array schemas to specify "items" with a
			// "type" field. The `[]T` shorthand publishes items of T;
			// publishing objects for every element made `[]string`
			// refuse its own strings.
			prop["items"] = promptArrayItems(f.elementType)
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
		// Published with the field's type (memql#5430): `"default": 5` on an
		// integer field. A field the converter did not check keeps its text.
		switch {
		case f.defaultSet && f.defaultValue != nil:
			prop["default"] = f.defaultValue
		case f.defaultVal != "":
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
