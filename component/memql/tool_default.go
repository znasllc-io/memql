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
// THE RULE: the quoted text is a literal of the field's declared type, read as
// JSON, since the value is published in the field's JSON-Schema input:
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
// A PROMPT field is held to the same rule. Its registry placement also admits
// an unquoted number, `@default(5)`, which is a literal of a numeric field --
// a whole one of an integer field -- and of nothing else: on a string field it
// is written quoted, `@default("5")`.
//
// WHY A QUOTED NUMBER STAYS LEGAL. The registry's one tool form is the quoted
// string, and every tool default in the tree is a quoted number on an integer
// field (`@default("10")`, dsl/memql/tools.memql and four more); no product
// bundle writes one, and no prompt anywhere declares a default. Accepting the
// quoted text when it parses as the declared type keeps every one of them
// loading, and refuses exactly the defaults that were never values of their
// field.
//
// The default is PUBLISHED with the field's type -- `"default": 10`, not
// `"default": "10"` -- so the schema the model reads agrees with itself, and
// toolSchemaDefaults hands a tool's handler the same value either way.

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

var (
	toolDefaultIntegerRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	toolDefaultNumberRe  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
)

// toolDefaultValue reads a tool field's @default text as a literal of the
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

// promptDefaultValue reads a prompt field's @default as a literal of the
// field's JSON-Schema type, returning the value to publish, or the refusal.
// Unlike a tool field's, it may have been written as an unquoted number.
func promptDefaultValue(prompt string, f toolField) (any, error) {
	schemaType := promptSchemaType(f.typeName)
	typeWord := f.typeName
	if f.elementType != "" {
		typeWord = "[]" + f.elementType
	}
	refuse := func(written, reason, fix string) error {
		return &FieldDefaultError{Construct: "prompt", Name: prompt, Field: f.name, Type: fieldTypeWord(typeWord, f.enumValues),
			Value: written, Reason: reason, Fix: fix, Code: PromptDefaultCode}
	}
	if f.defaultNumber {
		// An unquoted number is a literal of a numeric field and of nothing
		// else; its text reads as one exactly as the quoted form's does.
		if schemaType != "integer" && schemaType != "number" {
			return nil, refuse(f.defaultVal, fmt.Sprintf("is a number, and the field is a %s", schemaType),
				fmt.Sprintf("write it quoted, as in @default(%s)", strconv.Quote(f.defaultVal)))
		}
		value, reason, fix := literalOfType(schemaType, f.enumValues, f.defaultVal)
		if reason != "" {
			return nil, refuse(f.defaultVal, reason, fix)
		}
		return value, nil
	}
	value, reason, fix := literalOfType(schemaType, f.enumValues, f.defaultVal)
	if reason != "" {
		return nil, refuse(strconv.Quote(f.defaultVal), reason, fix)
	}
	return value, nil
}

// literalOfType reads text as a literal of a JSON-Schema type, one of the
// field's declared values when it has any. It returns the value, or -- when
// the text is not one -- why, and what to write instead.
func literalOfType(schemaType string, enum []string, text string) (value any, reason, fix string) {
	if len(enum) > 0 && !slices.Contains(enum, text) {
		return nil, "is not one of the field's values",
			fmt.Sprintf("write one of them, as in @default(%s)", strconv.Quote(enum[0]))
	}
	switch schemaType {
	case "string":
		return text, "", ""
	case "integer":
		if toolDefaultIntegerRe.MatchString(text) {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return n, "", ""
			}
			return nil, "is outside the integers a field can hold", `write one within 64 bits, as in @default("20")`
		}
		return nil, "is not an integer", `write one, as in @default("20")`
	case "number":
		if toolDefaultNumberRe.MatchString(text) {
			if v, err := strconv.ParseFloat(text, 64); err == nil && !math.IsInf(v, 0) {
				return v, "", ""
			}
			return nil, "is outside the numbers a field can hold", `write a finite one, as in @default("0.5")`
		}
		return nil, "is not a number", `write one, as in @default("0.5")`
	case "boolean":
		switch text {
		case "true":
			return true, "", ""
		case "false":
			return false, "", ""
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
