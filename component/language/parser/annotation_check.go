package parser

import (
	"sort"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/ast"
)

// annotation_check.go is the parse-time half of the annotation registry
// (component/language/annotations, memql#5359): the conversion of a parsed
// annotation into the Use the registry checks, and the check every construct
// parser and every field list runs. The registry decides which names, forms
// and keys are legal; the construct parsers keep only what a legal value
// MEANS.

// AnnotationUse converts one parsed annotation into the Use the registry
// checks: its name, the one argument form it was written in, and -- for
// keyword arguments -- the keys in the order written, each with its shape
// (bare or with a value), so a refusal names the first offending key as the
// author wrote it.
//
// A single bare `true` or `false` is FormBool, not a keyword argument named
// "false": the parser stores a bare word as a flag key, so without this
// reading `@default(false)` would look like a key.
func AnnotationUse(attr *ast.Attribute) annotations.Use {
	u := annotations.Use{Name: attr.Name}
	switch attr.Spelling {
	case ast.ArgsEmptyParens:
		u.Form = annotations.FormEmpty
		return u
	case ast.ArgsExclusion:
		u.Form = annotations.FormExclude
		return u
	}
	if attr.Value != nil {
		switch v := attr.Value.(type) {
		case string:
			u.Form = annotations.FormString
		case []string:
			if len(v) == 1 {
				u.Form = annotations.FormString
			} else {
				u.Form = annotations.FormStrings
			}
		case int, int64, float64:
			u.Form = annotations.FormNumber
		case bool:
			u.Form = annotations.FormBool
		case *ast.LambdaExpr:
			// @filter(row => ...), the edition-2026 trigger filter
			// (memql#5364): an expression, parsed as one rather than captured
			// as text.
			u.Form = annotations.FormExpression
		default:
			u.Form = annotations.FormObject
		}
		return u
	}
	if len(attr.Args) == 0 {
		u.Form = annotations.FormFlag
		return u
	}
	keys := writtenKeys(attr)
	if len(keys) == 1 && keys[0].Bare && (keys[0].Name == "true" || keys[0].Name == "false") {
		u.Form = annotations.FormBool
		return u
	}
	u.Form = annotations.FormKeywords
	u.Keys = keys
	return u
}

// writtenKeys returns the attribute's keyword keys in the order written, each
// with its shape. The parser records both (ast.Attribute.ArgKeys). An
// attribute built in Go carries only the Args map: its keys come back sorted,
// and a key whose value is the flag `true` counts as bare -- the parser's own
// reading of a bare key.
func writtenKeys(attr *ast.Attribute) []annotations.WrittenKey {
	if len(attr.ArgKeys) > 0 {
		out := make([]annotations.WrittenKey, 0, len(attr.ArgKeys))
		for _, k := range attr.ArgKeys {
			out = append(out, annotations.WrittenKey{Name: k.Name, Bare: k.Bare})
		}
		return out
	}
	names := make([]string, 0, len(attr.Args))
	for k := range attr.Args {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]annotations.WrittenKey, 0, len(names))
	for _, k := range names {
		flag, isBool := attr.Args[k].(bool)
		out = append(out, annotations.WrittenKey{Name: k, Bare: isBool && flag})
	}
	return out
}

// AnnotationUses converts a declaration's annotations, skipping nil entries.
func AnnotationUses(attrs []*ast.Attribute) []annotations.Use {
	out := make([]annotations.Use, 0, len(attrs))
	for _, a := range attrs {
		if a != nil {
			out = append(out, AnnotationUse(a))
		}
	}
	return out
}

// checkAnnotations is THE parse-time annotation gate: every construct parser
// and every field list hands it the annotations it read and the receiver they
// sit on. subject names the declaration for the message ("tool \"x\"",
// "args field \"y\""); the refusal itself names the receiver, the annotation
// and what to write, and ends with its stable code.
//
// The error points at the refused annotation's `@`, and carries the
// *annotations.Refusal as its cause so a caller can read the code with
// errors.As.
func (p *Parser) checkAnnotations(r annotations.Receiver, subject string, attrs []*ast.Attribute) error {
	kept := make([]*ast.Attribute, 0, len(attrs))
	for _, a := range attrs {
		if a != nil {
			kept = append(kept, a)
		}
	}
	uses := AnnotationUses(kept)
	ref := annotations.CheckAll(r, uses)
	if ref == nil {
		return nil
	}
	at := p.current
	for i := range uses {
		if annotations.CheckAll(r, uses[:i+1]) != nil {
			if tok, ok := p.attrTokens[kept[i]]; ok {
				at = tok
			}
			break
		}
	}
	return annotationRefusalError(at, subject, ref)
}

// annotationRefusalError renders a refusal as a parse error at tok. The token
// is carried as a position only, so the rendered error ends with the
// refusal's code rather than with a "(got ...)" suffix.
func annotationRefusalError(tok Token, subject string, ref *annotations.Refusal) *ParseError {
	msg := ref.Error()
	if subject != "" {
		msg = subject + ": " + msg
	}
	return &ParseError{Message: msg, Pos: tok.Pos, Line: tok.Line, Column: tok.Column, Cause: ref}
}

// functionReceivers maps the internal function receiver of a rewritten
// construct to the registry receiver its annotations are checked against,
// and the keyword the author wrote.
var functionReceivers = map[ReceiverType]struct {
	receiver annotations.Receiver
	keyword  string
}{
	ReceiverQuery:      {annotations.Query, "query"},
	ReceiverMutation:   {annotations.Mutation, "mutation"},
	ReceiverLogic:      {annotations.Logic, "logic"},
	ReceiverAutomation: {annotations.Automation, "automation"},
	ReceiverSpec:       {annotations.Spec, "spec"},
	ReceiverTool:       {annotations.Tool, "tool"},
	ReceiverBuiltin:    {annotations.Builtin, "builtin"},
	ReceiverPrompt:     {annotations.Prompt, "prompt"},
	ReceiverProvider:   {annotations.Provider, "provider"},
	ReceiverShape:      {annotations.Shape, "shape"},
	ReceiverPolicy:     {annotations.Policy, "policy"},
}
