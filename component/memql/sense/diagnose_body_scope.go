package sense

// diagnose_body_scope.go -- the scope and construct rules of a body written in
// statements (edition 2026, epic memql#5370), reported where the author breaks
// them.
//
// compiler.CheckBody is the load gate: a name read before it is bound or
// outside the block that binds it, a name bound twice, a bare argument read, a
// publish in a logic, a logic that does not end with a return. Diagnose runs
// that same check over the same parse, so an editor shows each refusal on its
// line rather than at boot, and the two cannot disagree about what a body may
// say. It runs the boot gate's config check too (component/memql/dslgate, with
// component/config.UnknownKey's text), which needs the allow-list and nothing
// of the workspace; the gate's other check, a bare call's callee, needs every
// spec and trait loaded and stays at boot. A body still written in the retired
// forms is the rewriter's until the flip and carries no statement body to
// check.

import (
	"strings"
	"unicode"

	"github.com/znasllc-io/memql/component/config"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/parser"
)

// bodyScopeDiagnostics is every problem CheckBody finds in the file's logic
// and automation bodies. lexed is the text the parse's positions are in, and
// place maps a diagnostic there onto the author's source.
func bodyScopeDiagnostics(file *parser.File, lexed string, place func(Diagnostic) Diagnostic) []Diagnostic {
	lines := strings.Split(lexed, "\n")
	var out []Diagnostic
	for _, n := range file.Definitions {
		def, ok := n.(*parser.FunctionDef)
		if !ok {
			continue
		}
		auto, ok := def.Body.(*parser.AutomationDef)
		if !ok || auto.Body == nil {
			continue
		}
		kind := "automation"
		if def.Type == parser.FunctionTypeLogic {
			kind = "logic"
		}
		var args []string
		if def.ArgsSchema != nil {
			for _, f := range def.ArgsSchema.Fields {
				if f != nil {
					args = append(args, f.Name)
				}
			}
		}
		for _, p := range compiler.CheckBody(kind, def.Name, args, auto.Body) {
			start := Position{Line: p.Line, Column: p.Col}
			out = append(out, place(Diagnostic{
				Range:    spanAt(start, wordLength(lines, start)),
				Severity: SeverityError,
				Message:  p.Error(),
				Code:     p.Code,
			}))
		}
		ast.WalkBody(auto.Body.Statements, func(s ast.BodyStatement) bool {
			for _, e := range ast.StatementExpressions(s) {
				ast.WalkV1(e, func(n ast.ExpressionNode) bool {
					m, ok := n.(*ast.MemberExpr)
					if !ok {
						return true
					}
					if root, ok := m.Object.(*ast.IdentExpr); ok && root.Name == "config" {
						if msg, unknown := config.UnknownKey(m.Field); unknown {
							start := Position{Line: root.Span.Line, Column: root.Span.Col}
							out = append(out, place(Diagnostic{
								Range:    spanAt(start, len("config.")+len([]rune(m.Field))),
								Severity: SeverityError,
								Message:  kind + " " + def.Name + ": " + msg + " [" + compiler.CodeBodyConfigUnknown + "]",
								Code:     compiler.CodeBodyConfigUnknown,
							}))
						}
					}
					return true
				})
			}
			return true
		})
	}
	return out
}

// wordLength is the length, in runes, of the word starting at start -- the
// name, keyword or root a problem is placed on -- and 1 where none starts
// there.
func wordLength(lines []string, start Position) int {
	if start.Line < 1 || start.Line > len(lines) {
		return 1
	}
	line := []rune(lines[start.Line-1])
	at := start.Column - 1
	if at < 0 || at >= len(line) {
		return 1
	}
	end := at
	for end < len(line) && (line[end] == '_' || unicode.IsLetter(line[end]) || unicode.IsDigit(line[end])) {
		end++
	}
	if end == at {
		return 1
	}
	return end - at
}
