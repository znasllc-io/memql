package memql

// prompt_converter.go bridges the langparser's *ast.PromptDecl AST
// node (introduced by memql#319 / sub-epic #309 / #306 child C) to
// the memql package's internal *promptDecl that LoadUnifiedPrompts
// consumes. The hand-rolled parsePromptMemQL (prompt_parser.go) is
// unreferenced from production after this child lands but kept for
// its tests until sub-epic #306 child D's final deletion.
//
// Semantics mirror parsePromptMemQL one-for-one:
//
//   * Prompt-level annotations: @enabled / @disabled (no-ops at the
//     converter -- they're loader-pipeline flags), @description,
//     @defaultProvider, @templateFile. Unknown annotations are
//     tolerated silently to match the hand-rolled parser's
//     drain-and-skip default branch.
//   * Body fields lower to the internal toolField slice (prompts +
//     tools share the same in-package field type so the JSON-schema
//     compilation path on promptDecl can stay unchanged). The `[]T`
//     shorthand surfaces as typeName="array" + elementType=T --
//     same shape parsePromptMemQL produces, which the JSON-schema
//     compiler keys on at registration time.
//   * Field annotations: @required (acted on), @description /
//     @enum / @default (carried through to the JSON-schema layer).
//     Unknown field annotations are tolerated silently.
//
// What this converter does NOT do: resolve the template sidecar,
// compile the JSON schema, or register the prompt. Those steps stay
// in LoadUnifiedPrompts because they need the raw file's directory
// path (sidecar resolution) and the engine's PromptRegistry +
// partials template (registration + parse). Mirrors how the
// builtin / shape / provider converters scope themselves narrowly
// to AST -> registry-type translation.

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// promptDeclToPromptDecl converts a langparser PromptDecl into the
// in-package *promptDecl that LoadUnifiedPrompts consumes. The two
// types share the same name (PromptDecl) across package boundaries
// -- the function name reads `promptDeclToPromptDecl` because the
// public AST type and the unexported internal type happen to align.
func promptDeclToPromptDecl(decl *languageParser.PromptDecl, origin string) (*promptDecl, error) {
	if decl == nil {
		return nil, fmt.Errorf("prompt decl is nil")
	}
	if strings.TrimSpace(decl.Name) == "" {
		return nil, fmt.Errorf("%s: prompt name is required", origin)
	}

	out := &promptDecl{
		name: decl.Name,
	}

	for _, attr := range decl.Attributes {
		switch attr.Name {
		case "enabled", "disabled":
			// Lifecycle flags read elsewhere; no converter effect.
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @description expects a string value", origin)
			}
			out.description = val
		case "defaultProvider":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @defaultProvider expects a string value", origin)
			}
			out.defaultProvider = val
		case "templateFile":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @templateFile expects a string value", origin)
			}
			out.templateFile = val
		default:
			// Unknown annotation -- tolerated silently. Mirrors
			// parsePromptMemQL's drain-and-skip default branch.
		}
	}

	for _, field := range decl.Fields {
		tf, err := promptFieldToToolField(field, origin)
		if err != nil {
			return nil, err
		}
		out.fields = append(out.fields, tf)
	}

	return out, nil
}

// promptFieldToToolField lowers one langparser PromptField into the
// in-package toolField shape. The `[]T` shorthand becomes
// typeName="array" + elementType=T, matching parsePromptMemQL's
// output -- the JSON-schema compiler on promptDecl keys on this
// pair, so the converter has to preserve it exactly.
func promptFieldToToolField(field *languageParser.PromptField, origin string) (toolField, error) {
	if field == nil {
		return toolField{}, fmt.Errorf("%s: prompt field is nil", origin)
	}
	if strings.TrimSpace(field.Name) == "" {
		return toolField{}, fmt.Errorf("%s: prompt field name is required", origin)
	}

	tf := toolField{
		name:     field.Name,
		required: field.Required,
	}

	// Type: parser emits Type="[]X" for the array shorthand; lower
	// that to typeName="array" + elementType=X.
	if strings.HasPrefix(field.Type, "[]") {
		tf.typeName = "array"
		tf.elementType = strings.TrimPrefix(field.Type, "[]")
	} else {
		tf.typeName = field.Type
	}
	if tf.typeName == "" {
		return toolField{}, fmt.Errorf("%s: prompt field %q is missing a type", origin, field.Name)
	}

	for _, attr := range field.Attributes {
		switch attr.Name {
		case "required":
			// Already captured into field.Required by the parser.
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return toolField{}, fmt.Errorf("%s: field %q @description expects a string value", origin, field.Name)
			}
			tf.description = val
		case "default":
			// parsePromptMemQL stores @default as a string (the
			// JSON-schema layer round-trips it as `default: <value>`).
			// Accept any scalar and stringify -- the hand-rolled
			// parser was likewise lenient.
			tf.defaultVal = stringifyAttrValue(attr.Value)
		case "enum":
			// Multi-value or single string. parseAttribute may surface
			// the args as either a single string or a list under
			// Args["values"] depending on grammar -- normalise both.
			tf.enumValues = enumValuesFromAttr(attr)
		default:
			// Unknown field annotation -- tolerated silently. Matches
			// parsePromptMemQL's "skip args + continue" default.
		}
	}

	return tf, nil
}

// stringifyAttrValue coerces an attribute value to a string so the
// converter can hand it to the JSON-schema layer as a default. The
// langparser attribute surface carries strings as `string`, numbers
// as float64, and bools as bool -- this preserves parsePromptMemQL's
// "stored as string" shape regardless of literal form.
func stringifyAttrValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// Trim trailing zeros on whole numbers so 10 doesn't render
		// as "10.000000"; otherwise float-format.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// enumValuesFromAttr normalises an @enum attribute value into a
// string slice. The langparser surfaces multi-arg attributes via
// the Args map; the canonical key for the enum's value list is
// "values" today, but be defensive and also accept a slice in
// Value directly.
func enumValuesFromAttr(attr *languageParser.Attribute) []string {
	if attr == nil {
		return nil
	}
	// Single-string Value (e.g. @enum("a") -> Value="a").
	if s, ok := attr.Value.(string); ok && s != "" {
		return []string{s}
	}
	// Slice Value: any []string or []any in Value.
	switch list := attr.Value.(type) {
	case []string:
		return append([]string(nil), list...)
	case []any:
		out := make([]string, 0, len(list))
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	// Args map: prefer "values" key (parseAttribute's multi-arg
	// shape for @enum("a", "b", "c")).
	if vals, ok := attr.Args["values"]; ok {
		switch list := vals.(type) {
		case []string:
			return append([]string(nil), list...)
		case []any:
			out := make([]string, 0, len(list))
			for _, v := range list {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}
