package memql

// tool_default.go -- a tool field's or a prompt field's @default, held to a
// literal of the field's declared type at load (memql#5430).
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
// THE RULE: the default's text converts to the field's declared type the way
// a tool's default has always been converted at call time
// (coerceSchemaDefault, strconv), and is refused only when it does not:
//
//	string   any text (the text itself)
//	integer  what strconv.ParseInt(text, 10, 64) reads: "10", "+10", "010"
//	number   what strconv.ParseFloat(text, 64) reads: "2.5", ".5", "1e3"
//	boolean  what strconv.ParseBool reads: "true", "false", "1", "0", "t", "TRUE"
//	array    a JSON array: @default("[]")
//	object   a JSON object: @default("{}")
//
// and, when the field declares its values (`enum(...)` or @enum), one of them.
// Every spelling the conversion read before still loads -- the language
// freeze retires a spelling that worked only through a deprecation window --
// and what it could not read, the text a handler used to receive as a raw
// string, is refused. An array or an object was never converted, so a
// default of either kind is read as JSON: its value is published at last,
// and text that is no array or object is refused.
//
// A PROMPT field is held to the same rule. Its registry placement also admits
// an unquoted number, `@default(5)`, which is read as the text it was written
// as -- `@default(1e3)` and `@default("1e3")` are one default, and an integer
// keeps every digit.
//
// The default is PUBLISHED with the field's type and in its canonical form --
// `"default": 10` for "+10", `true` for "TRUE" -- so the schema the model
// reads agrees with itself, and toolSchemaDefaults hands a tool's handler the
// value the conversion always produced. A number field's NaN or infinity has
// no JSON form, so its text is published, and converts at call time as before.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// The @default refusal codes. Stable: the load report, memqllint and the
// conformance corpus key on them.
const (
	// ToolDefaultCode: a tool field's @default is not a literal of the
	// field's type.
	ToolDefaultCode = "tool_default_type"
	// PromptDefaultCode: a prompt field's @default is not a literal of the
	// field's type.
	PromptDefaultCode = "prompt_default_type"
)

// FieldDefaultError refuses a tool field's or a prompt field's @default: the
// construct, the field, its type as written, the default as written, why, and
// what to write instead, with the rule id last in brackets.
type FieldDefaultError struct {
	// Construct is "tool" or "prompt"; Name is the construct's name.
	Construct string
	Name      string
	Field     string
	Type      string
	// Value is the default as written: quoted text, or an unquoted number.
	Value  string
	Reason string
	Fix    string
	Code   string
}

// Error prints the refusal as one sentence:
//
//	tool "listTickets" field "limit" (integer): @default("twenty") is not an
//	integer -- write one, as in @default("20") [tool_default_type]
func (e *FieldDefaultError) Error() string {
	return fmt.Sprintf("%s %q field %q (%s): @default(%s) %s -- %s [%s]",
		e.Construct, e.Name, e.Field, e.Type, e.Value, e.Reason, e.Fix, e.Code)
}

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *FieldDefaultError) RuleCode() string { return e.Code }

// toolDefaultValue reads a tool field's @default text as a value of the
// field's JSON-Schema type (schemaType: the type toolDeclToTool publishes),
// returning the value to publish, or the refusal.
func toolDefaultValue(tool string, f ast.ToolFieldDecl, schemaType string) (any, error) {
	value, reason, fix := literalOfType(schemaType, f.EnumValues, f.Default)
	if reason == "" {
		return value, nil
	}
	return nil, &FieldDefaultError{Construct: "tool", Name: tool, Field: f.Name, Type: fieldTypeWord(f.Type, f.EnumValues),
		Value: strconv.Quote(f.Default), Reason: reason, Fix: fix, Code: ToolDefaultCode}
}

// promptDefaultValue reads a prompt field's @default as a value of the field's
// JSON-Schema type, returning the value to publish, or the refusal. An
// unquoted number is read as the text it was written as, so it and its quoted
// spelling are one default.
func promptDefaultValue(prompt string, f toolField) (any, error) {
	typeWord := f.typeName
	if f.elementType != "" {
		typeWord = "[]" + f.elementType
	}
	value, reason, fix := literalOfType(promptSchemaType(f.typeName), f.enumValues, f.defaultVal)
	if reason == "" {
		return value, nil
	}
	written := strconv.Quote(f.defaultVal)
	if f.defaultNumber {
		written = f.defaultVal
	}
	return nil, &FieldDefaultError{Construct: "prompt", Name: prompt, Field: f.name, Type: fieldTypeWord(typeWord, f.enumValues),
		Value: written, Reason: reason, Fix: fix, Code: PromptDefaultCode}
}

// literalOfType converts text to a value of a JSON-Schema type, one of the
// field's declared values when it has any, by the conversion a tool's default
// has always had at call time (coerceSchemaDefault). It returns the value to
// publish, or -- when the conversion does not read the text -- why, and what
// to write instead.
func literalOfType(schemaType string, enum []string, text string) (value any, reason, fix string) {
	if len(enum) > 0 && !slices.Contains(enum, text) {
		return nil, "is not one of the field's values",
			fmt.Sprintf("write one of them, as in @default(%s)", strconv.Quote(enum[0]))
	}
	switch schemaType {
	case "string":
		return text, "", ""
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		switch {
		case err == nil:
			return n, "", ""
		case errors.Is(err, strconv.ErrRange):
			return nil, "is outside the integers a field can hold", `write one within 64 bits, as in @default("20")`
		}
		return nil, "is not an integer", `write one, as in @default("20")`
	case "number":
		// An integer keeps every digit: a float64 would publish
		// 9007199254740993 as ...992.
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, "", ""
		}
		f, err := strconv.ParseFloat(text, 64)
		switch {
		case err == nil && (math.IsNaN(f) || math.IsInf(f, 0)):
			return text, "", "" // no JSON form; converts at call time, as it always did
		case err == nil:
			return f, "", ""
		case errors.Is(err, strconv.ErrRange):
			return nil, "is outside the numbers a field can hold", `write a smaller one, as in @default("0.5")`
		}
		return nil, "is not a number", `write one, as in @default("0.5")`
	case "boolean":
		if b, err := strconv.ParseBool(text); err == nil {
			return b, "", ""
		}
		return nil, "is not true or false", `write @default("true") or @default("false")`
	case "array":
		var v []any
		if strings.HasPrefix(strings.TrimSpace(text), "[") && json.Unmarshal([]byte(text), &v) == nil {
			return v, "", ""
		}
		return nil, "is not a JSON array", `write one, as in @default("[]")`
	case "object":
		var v map[string]any
		if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal([]byte(text), &v) == nil {
			return v, "", ""
		}
		return nil, "is not a JSON object", `write one, as in @default("{}")`
	}
	return nil, fmt.Sprintf("has no literal form for a field of JSON type %q", schemaType), "drop the @default"
}

// fieldTypeWord is a field's type as a refusal names it: the type as written,
// with the values it declares, if any -- `enum("open", "closed")` for a string
// field, `integer @enum("1", "2")` otherwise.
func fieldTypeWord(typ string, enum []string) string {
	if len(enum) == 0 {
		return typ
	}
	quoted := make([]string, len(enum))
	for i, v := range enum {
		quoted[i] = strconv.Quote(v)
	}
	values := strings.Join(quoted, ", ")
	if strings.EqualFold(typ, "string") {
		return "enum(" + values + ")"
	}
	return typ + " @enum(" + values + ")"
}
