// statement_bodies.go -- what a body written in statements may read and call,
// checked against the corpus (epic memql#5370).
//
// Two expressions in a statement body load, and then either read nothing or
// fail the first time they run, unless a gate refuses them at load:
//
//   - `config.<key>` reads the allow-listed configuration envelope
//     (component/config PolicyExposableConfig), and a key the list does not
//     hold -- however it is spelled -- reads absent at run time, silently.
//   - A bare call in an expression names a catalog function or a predicate: a
//     spec or trait the corpus declares. Anything else is refused only when
//     the expression runs ("not a function or a predicate known here"). A
//     construct call is written with its kind, as a statement of its own
//     (`x := query getUser(...)`), which the parser and the scope check
//     already hold.
//
// Both need what one body cannot see -- the allow-list, and the specs and
// traits declared anywhere in the corpus -- so they are answered here, over
// the whole corpus at boot (ScanFiles), rather than in the compiler's scope
// check (compiler.CheckBody), which answers every rule a body can decide
// alone. Like the sub-automation gate, "declared nowhere" is the violation,
// so this gate is right only over the merged corpus ScanFiles is given.
//
// A body the parser refuses (a retired form among them) is passed over: the
// loader reports the refusal, and StatementBodiesRead names only the bodies
// the gates read.
package dslgate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/config"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/functions"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// The gate ids. Their details end with the statement language's codes (D24),
// compiler.CodeBodyConfigUnknown and compiler.CodeBodyCallUnknown.
const (
	GateStatementConfigKey   Gate = "statement-config-key"
	GateStatementUnknownCall Gate = "statement-unknown-call"
)

// statementHeader finds a logic or automation declaration that opens a body.
var statementHeader = regexp.MustCompile(`(?m)^[ \t]*(logic|automation)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// scanStatementBodies reports the config reads and bare calls of every body
// written in statements that the corpus cannot answer.
func scanStatementBodies(files []SourceFile) []Violation {
	predicates, constructs := map[string]bool{}, map[string]string{}
	for _, f := range files {
		for _, m := range declLineRe.FindAllStringSubmatch(codeOnly(f.Content), -1) {
			name := m[2]
			if m[3] != "" {
				name = m[3] // two-identifier signature: `spec <bound> <name>`
			}
			switch m[1] {
			case "spec", "trait":
				predicates[name] = true
			case "query", "mutate", "mutation", "logic", "builtin":
				constructs[name] = strings.Replace(m[1], "mutate", "mutation", 1)
			}
		}
	}
	var out []Violation
	eachStatementBody(files, func(f SourceFile, kind, name string, startLine int, body *ast.Body) {
		report := func(gate Gate, sp ast.Span, detail string) {
			out = append(out, Violation{Gate: gate, File: f.Path, Line: startLine + sp.Line - 1,
				Kind: kind, Construct: name, Detail: detail})
		}
		ast.WalkBody(body.Statements, func(s ast.BodyStatement) bool {
			for _, e := range ast.StatementExpressions(s) {
				ast.WalkV1(e, func(n ast.ExpressionNode) bool {
					switch x := n.(type) {
					case *ast.MemberExpr:
						if root, ok := x.Object.(*ast.IdentExpr); ok && root.Name == "config" {
							if msg, unknown := config.UnknownKey(x.Field); unknown {
								report(GateStatementConfigKey, x.Span, msg+" ["+compiler.CodeBodyConfigUnknown+"]")
							}
						}
					case *ast.CallExpr:
						if x.Kind != "" || x.Receiver != nil {
							break
						}
						if _, fn := functions.Lookup(x.Name); fn || predicates[x.Name] {
							break
						}
						remedy := "call a catalog function or a spec or trait the corpus declares"
						if k, isConstruct := constructs[x.Name]; isConstruct {
							remedy = fmt.Sprintf("%s is a %s, which is called with its kind as a statement of its own: `x := %s %s(...)`, then read x",
								x.Name, k, k, x.Name)
						}
						report(GateStatementUnknownCall, x.Span, fmt.Sprintf(
							"`%s(...)` is not a function or a predicate known here, so the expression fails when it runs: %s [%s]",
							x.Name, remedy, compiler.CodeBodyCallUnknown))
					}
					return true
				})
			}
			return true
		})
	})
	return out
}

// StatementBodiesRead is the statement-body gates' coverage: "<file> <name>"
// for every logic and automation in files whose body they read. A body the
// parser refuses is not read, so a caller that
// knows which statement bodies the corpus holds compares the two -- a body the
// gates could not read then cannot pass for one they found clean.
func StatementBodiesRead(files []SourceFile) []string {
	var out []string
	eachStatementBody(files, func(f SourceFile, _, name string, _ int, _ *ast.Body) {
		out = append(out, f.Path+" "+name)
	})
	return out
}

// eachStatementBody calls visit with every logic and automation in files
// whose body is written in statements, and the line its declaration starts
// on. Each declaration is parsed alone, from its keyword to its closing brace.
func eachStatementBody(files []SourceFile, visit func(f SourceFile, kind, name string, startLine int, body *ast.Body)) {
	for _, f := range files {
		if !strings.Contains(f.Content, "logic") && !strings.Contains(f.Content, "automation") {
			continue
		}
		view := languageParser.BlankCommentsAndStrings(f.Content)
		for _, loc := range statementHeader.FindAllStringSubmatchIndex(view, -1) {
			closeAt := closingBraceAt(view, loc[1]-1)
			if closeAt < 0 {
				continue // the loader reports the unbalanced construct
			}
			startLine := 1 + strings.Count(f.Content[:loc[0]], "\n")
			kind, name := f.Content[loc[2]:loc[3]], f.Content[loc[4]:loc[5]]
			pf, err := languageParser.ParseFile(f.Content[loc[0] : closeAt+1])
			if err != nil {
				continue // a refusal the loader reports
			}
			for _, d := range pf.Definitions {
				fn, ok := d.(*ast.FunctionDef)
				if !ok || fn.Name != name {
					continue
				}
				if auto, ok := fn.Body.(*ast.AutomationDef); ok && auto.Body != nil {
					visit(f, kind, name, startLine, auto.Body)
				}
			}
		}
	}
}

// closingBraceAt is the index of the `}` closing the `{` at open in a view
// with comments and strings blanked, or -1.
func closingBraceAt(view string, open int) int {
	depth := 0
	for i := open; i < len(view); i++ {
		switch view[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
