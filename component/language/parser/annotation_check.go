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
// keyword arguments -- the keys, sorted (the parser keeps them in a map).
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
	case ast.ArgsRawExpression:
		u.Form = annotations.FormExpression
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
		default:
			u.Form = annotations.FormObject
		}
		return u
	}
	if len(attr.Args) == 0 {
		u.Form = annotations.FormFlag
		return u
	}
	if len(attr.Args) == 1 {
		for k, v := range attr.Args {
			if b, isBool := v.(bool); isBool && b && (k == "true" || k == "false") {
				u.Form = annotations.FormBool
				return u
			}
		}
	}
	u.Form = annotations.FormKeywords
	for k := range attr.Args {
		u.Keys = append(u.Keys, k)
	}
	sort.Strings(u.Keys)
	return u
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
	ReceiverMutation:   {annotations.Mutation, "mutate"},
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
