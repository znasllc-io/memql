package memql

// tool_default.go -- a tool field's @default, held to a literal of the field's
// declared type at load (memql#5430).
//
// The annotation registry admits one quoted string on a tool field --
// `@default("20")` -- so `@default(20)`, `@default(x)`, `@default(args.x)`,
// `@default({a: 1})` and `@default(null)` are refused at parse, by form. What
// was never checked is the TEXT inside the quotes. `limit integer
// @default("twenty")` loaded, advertised `"default": "twenty"` to the model on
// an integer field, and handed "twenty" to the handler whenever the model left
// the field out: coerceSchemaDefault falls back to the raw string when it
// cannot coerce. That is memql#1673's shape exactly -- calendarCreate's
// `"false"` default reached a bool argument as a string and failed the
// mutation -- one annotation at a time.
//
// THE RULE: the quoted text is a literal of the field's declared type, read as
// JSON, since the value is published in the tool's JSON-Schema input:
//
//	string   any text (the text itself)
//	integer  an integer literal: -?(0|[1-9][0-9]*), within int64
//	number   a number literal: -?(0|[1-9][0-9]*)(.[0-9]+)?([eE][+-]?[0-9]+)?
//	boolean  true or false
//	array    a JSON array: @default("[]")
//	object   a JSON object: @default("{}")
//
// and, when the field declares its values (`enum(...)` or @enum), one of them.
//
// WHY A QUOTED NUMBER STAYS LEGAL. The registry's one form is the quoted
// string, and every tool default in the tree is a quoted number on an integer
// field (`@default("10")`, dsl/memql/tools.memql and four more); no product
// bundle writes one. Accepting the quoted text when it parses as the declared
// type keeps every one of them loading, and refuses exactly the defaults that
// were never values of their field.
//
// The default is PUBLISHED with the field's type -- `"default": 10`, not
// `"default": "10"` -- so the schema the model reads agrees with itself, and
// toolSchemaDefaults hands the handler the same value either way.

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// ToolDefaultCode is the rule id of a tool @default whose text is not a
// literal of the field's type. Stable: the load report, memqllint and the
// conformance corpus key on it.
const ToolDefaultCode = "tool_default_type"

// ToolDefaultError refuses a tool field's @default: the tool, the field, its
// type as written, the default as written, why, and what to write instead,
// with the rule id last in brackets.
type ToolDefaultError struct {
	Tool   string
	Field  string
	Type   string
	Value  string
	Reason string
	Fix    string
}

// Error prints the refusal as one sentence:
//
//	tool "listTickets" field "limit" (integer): @default("twenty") is not an
//	integer -- write one, as in @default("20") [tool_default_type]
func (e *ToolDefaultError) Error() string {
	return fmt.Sprintf("tool %q field %q (%s): @default(%s) %s -- %s [%s]",
		e.Tool, e.Field, e.Type, strconv.Quote(e.Value), e.Reason, e.Fix, ToolDefaultCode)
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *ToolDefaultError) RuleCode() string { return ToolDefaultCode }

var (
	toolDefaultIntegerRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	toolDefaultNumberRe  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
)

// toolDefaultValue reads a tool field's @default text as a literal of the
// field's JSON-Schema type (schemaType: the type toolDeclToTool publishes),
// returning the value to publish, or the refusal.
func toolDefaultValue(tool string, f ast.ToolFieldDecl, schemaType string) (any, error) {
	text := f.Default
	refuse := func(reason, fix string) error {
		return &ToolDefaultError{Tool: tool, Field: f.Name, Type: toolFieldTypeWord(f), Value: text, Reason: reason, Fix: fix}
	}

	if len(f.EnumValues) > 0 && !slices.Contains(f.EnumValues, text) {
		return nil, refuse("is not one of the field's values",
			fmt.Sprintf("write one of them, as in @default(%s)", strconv.Quote(f.EnumValues[0])))
	}

	switch schemaType {
	case "string":
		return text, nil
	case "integer":
		if toolDefaultIntegerRe.MatchString(text) {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n, nil
			}
			return nil, refuse("is outside the integers a field can hold", `write one within 64 bits, as in @default("20")`)
		}
		return nil, refuse("is not an integer", `write one, as in @default("20")`)
	case "number":
		if toolDefaultNumberRe.MatchString(text) {
			if v, err := strconv.ParseFloat(text, 64); err == nil && !math.IsInf(v, 0) {
				return v, nil
			}
			return nil, refuse("is outside the numbers a field can hold", `write a finite one, as in @default("0.5")`)
		}
		return nil, refuse("is not a number", `write one, as in @default("0.5")`)
	case "boolean":
		switch text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, refuse("is not true or false", `write @default("true") or @default("false")`)
	case "array":
		var v []any
		if strings.HasPrefix(strings.TrimSpace(text), "[") && json.Unmarshal([]byte(text), &v) == nil {
			return v, nil
		}
		return nil, refuse("is not a JSON array", `write one, as in @default("[]")`)
	case "object":
		var v map[string]any
		if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal([]byte(text), &v) == nil {
			return v, nil
		}
		return nil, refuse("is not a JSON object", `write one, as in @default("{}")`)
	}
	return nil, refuse(fmt.Sprintf("has no literal form for a field of JSON type %q", schemaType), "drop the @default")
}

// toolFieldTypeWord is a tool field's type as a refusal names it: the type as
// written, with the values it declares, if any -- `enum("open", "closed")`
// for a string field, `integer @enum("1", "2")` otherwise.
func toolFieldTypeWord(f ast.ToolFieldDecl) string {
	if len(f.EnumValues) == 0 {
		return f.Type
	}
	quoted := make([]string, len(f.EnumValues))
	for i, v := range f.EnumValues {
		quoted[i] = strconv.Quote(v)
	}
	values := strings.Join(quoted, ", ")
	if strings.EqualFold(f.Type, "string") {
		return "enum(" + values + ")"
	}
	return f.Type + " @enum(" + values + ")"
}
