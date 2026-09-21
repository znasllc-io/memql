package main

// deprecatedforms.go -- a deprecated language form, fixed where the author is
// looking (memql#5390).
//
// Sense warns on each use of a deprecated form still inside its window, and the
// parser refuses a use past it; both diagnostics carry the form's rule as their
// code (component/language/deprecation), and toLSPDiagnostic tags both
// Deprecated. This file offers the quick fix on either: "Rewrite as
// `[]string`", the replacement written over the spelling and nothing else.
//
// WHICH DIAGNOSTIC IS OURS is decided by its source, its message and its range,
// never by its code: glsp v0.2.2 delivers the code a client echoes back in a
// code-action request as nil (languageline.go says how), so a fix keyed on the
// code never fires against a real client while a Go-built request passes. The
// uses are re-scanned from the buffer the server holds, and a request
// diagnostic belongs to a use when this server published it, it carries the
// form's warning or refusal, and its range touches the use.

import (
	"strings"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/language/deprecation"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// deprecatedFormCodeActions answers a code-action request with one preferred
// quick fix per use of a deprecated form that one of the request's diagnostics
// is about.
func (s *server) deprecatedFormCodeActions(params *protocol.CodeActionParams) []protocol.CodeAction {
	if !wantsQuickFix(params.Context.Only) || len(params.Context.Diagnostics) == 0 {
		return nil
	}
	uri := params.TextDocument.URI
	text, ok := s.docs.get(uri)
	if !ok {
		return nil
	}
	var actions []protocol.CodeAction
	for _, use := range langparser.ScanDeprecatedUses(text) {
		form, ok := deprecation.Lookup(use.Rule)
		if !ok {
			continue
		}
		replacement, ok := use.Replacement()
		if !ok {
			continue
		}
		rng := toLSPRange(text, deprecatedUseRange(use))
		var resolved []protocol.Diagnostic
		for _, d := range params.Context.Diagnostics {
			if !aboutDeprecatedUse(d, form, rng) {
				continue
			}
			// The diagnostic as published: the client matches an action to the
			// diagnostic it holds, whose code glsp dropped on the way in.
			d.Code = &protocol.IntegerOrString{Value: form.Rule}
			resolved = append(resolved, d)
		}
		if len(resolved) == 0 {
			continue
		}
		actions = append(actions, protocol.CodeAction{
			Title:       "Rewrite as `" + replacement + "`",
			Kind:        ptr(protocol.CodeActionKindQuickFix),
			Diagnostics: resolved,
			IsPreferred: ptr(true),
			Edit: &protocol.WorkspaceEdit{Changes: map[protocol.DocumentUri][]protocol.TextEdit{
				uri: {{Range: rng, NewText: replacement}},
			}},
		})
	}
	return actions
}

// deprecatedUseRange is the Sense range of a use: the spelling as written,
// which is the range Sense's warning on it carries.
func deprecatedUseRange(use langparser.DeprecatedUse) sense.Range {
	endLine, endCol := use.End()
	return sense.Range{
		Start: sense.Position{Line: use.Line, Column: use.Column},
		End:   sense.Position{Line: endLine, Column: endCol},
	}
}

// aboutDeprecatedUse reports whether a request diagnostic is this server's
// diagnostic about a use of form at rng: its source, the form's warning (a use
// inside its window) or its refusal (a use past it), and a range touching the
// use.
func aboutDeprecatedUse(d protocol.Diagnostic, form deprecation.Form, rng protocol.Range) bool {
	if d.Source == nil || *d.Source != lsName {
		return false
	}
	if d.Message != form.Warning() && !strings.Contains(d.Message, form.Refusal()) {
		return false
	}
	return !positionBefore(d.Range.End, rng.Start) && !positionBefore(rng.End, d.Range.Start)
}

// positionBefore reports whether a is strictly before b.
func positionBefore(a, b protocol.Position) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Character < b.Character
}
