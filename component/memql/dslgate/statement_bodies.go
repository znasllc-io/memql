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
		// A two-identifier signature (`spec <bound> <name>`) declares its
		// second identifier (declFact.name).
		for _, d := range factsOf(f.Content).declarations() {
			switch d.kind {
			case "spec", "trait":
				predicates[d.name] = true
			case "query", "mutation", "logic", "builtin":
				constructs[d.name] = d.kind
			}
		}
	}
	var out []Violation
	eachStatementBody(files, func(f SourceFile, kind, name string, startLine int, def *ast.AutomationDef) {
		report := func(gate Gate, sp ast.Span, detail string) {
			out = append(out, Violation{Gate: gate, File: f.Path, Line: startLine + sp.Line - 1,
				Kind: kind, Construct: name, Detail: detail})
		}
		check := func(e ast.ExpressionNode) {
			ast.WalkV1(e, func(n ast.ExpressionNode) bool {
				switch x := n.(type) {
				case *ast.MemberExpr:
					if root, ok := x.Object.(*ast.IdentExpr); ok && root.Name == "config" {
						if msg, unknown := config.UnknownKey(x.Field); unknown {
							report(GateStatementConfigKey, x.Span, msg+" ["+compiler.CodeBodyConfigUnknown+"]")
						}
					}
				case *ast.CallExpr:
					if x.Kind != "" {
						break
					}
					if x.Receiver != nil {
						if detail := unknownMethod(x.Name); detail != "" {
							report(GateStatementUnknownCall, x.Span, detail)
						} else if detail := methodCallMisfit(x); detail != "" {
							report(GateStatementUnknownCall, x.Span, detail)
						}
						break
					}
					if fn, isFunction := functions.Lookup(x.Name); isFunction {
						if misfit := callShapeMisfit(x, fn); misfit != "" {
							report(GateStatementUnknownCall, x.Span, misfit+", so the expression fails when it runs ["+compiler.CodeBodyCallUnknown+"]")
						}
						break
					}
					if predicates[x.Name] {
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
		// An automation's header lambdas are expressions of the automation
		// too, evaluated in process by the same evaluator its conditions are
		// (memql#5426): a trigger filter calling a name nothing declares
		// loaded, and refused every fire.
		if def.Trigger != nil && def.Trigger.FilterLambda != nil {
			check(def.Trigger.FilterLambda)
		}
		if def.Loop != nil && def.Loop.Until != nil {
			check(def.Loop.Until)
		}
		ast.WalkBody(def.Body.Statements, func(s ast.BodyStatement) bool {
			for _, e := range ast.StatementExpressions(s) {
				check(e)
			}
			return true
		})
	})
	return out
}

// unknownMethod is the report for a method name no receiver has, or "" when a
// list or a string has it -- exactly the question the in-process evaluator
// asks before it evaluates the receiver (expr_eval.go's method), so a name it
// would refuse on every run is refused at load instead (memql#5426).
func unknownMethod(name string) string {
	if _, onList := functions.Method(functions.TypeList, name); onList {
		return ""
	}
	if _, onString := functions.Method(functions.TypeString, name); onString {
		return ""
	}
	if replacement, retired := functions.RetiredMethods()[functions.TypeList+"."+name]; retired {
		return fmt.Sprintf("`.%s()` is retired in edition 2026, so the expression fails when it runs: write %s [%s]",
			name, replacement, compiler.CodeBodyCallUnknown)
	}
	return fmt.Sprintf("`.%s()` is not a method of a list or a string, so the expression fails when it runs: call a method the function catalog lists [%s]",
		name, compiler.CodeBodyCallUnknown)
}

// methodCallMisfit is the report for a call of a method the evaluator refuses
// on every run although the name is one, or "" when the call may run. Two
// things are decided before a receiver's value exists, so they are decided at
// load too (memql#5426 review):
//
//   - the call's shape -- how many arguments, and which are lambdas -- which
//     the evaluator checks against the catalog entry before it evaluates the
//     receiver (exprCheckShape): `args.items.any()` fails whatever the items
//     are. A name a list and a string both have fits when it fits either.
//   - a string literal's methods: `"abc".sum()` is a list method called on a
//     value whose type the source already says.
//
// A lambda's parameter count is not checked: the evaluator reads it from a
// table of its own (list.reduce takes two), which the catalog does not state.
func methodCallMisfit(x *ast.CallExpr) string {
	listFn, onList := functions.Method(functions.TypeList, x.Name)
	strFn, onString := functions.Method(functions.TypeString, x.Name)
	if lit, ok := ast.Unparen(x.Receiver).(*ast.LiteralExpr); ok {
		if _, isString := lit.Value.(string); isString {
			if !onString {
				return fmt.Sprintf("`.%s()` is a list method and `%s` is a string, so the expression fails when it runs: call it on a list, or call a string method [%s]",
					x.Name, ast.FormatExpr(x.Receiver), compiler.CodeBodyCallUnknown)
			}
			onList = false
		}
	}
	misfit := ""
	for _, entry := range []struct {
		fn functions.Function
		ok bool
	}{{listFn, onList}, {strFn, onString}} {
		if !entry.ok {
			continue
		}
		m := callShapeMisfit(x, entry.fn)
		if m == "" {
			return ""
		}
		if misfit == "" {
			misfit = m
		}
	}
	if misfit == "" {
		return ""
	}
	return misfit + ", so the expression fails when it runs [" + compiler.CodeBodyCallUnknown + "]"
}

// callShapeMisfit says how a call does not fit a catalog entry's parameters,
// or "" when it does -- the evaluator's shape check (exprCheckShape) as far as
// the catalog states it: positional arguments only, a count between the
// required parameters and all of them, and a lambda exactly where the entry
// takes one.
func callShapeMisfit(x *ast.CallExpr, fn functions.Function) string {
	if len(x.Named) > 0 {
		return fmt.Sprintf("%s takes positional arguments, not named ones", fn.Signature())
	}
	required := 0
	for _, p := range fn.Params {
		if !p.Optional {
			required++
		}
	}
	if n := len(x.Args); n < required || n > len(fn.Params) {
		want := fmt.Sprintf("%d", required)
		if required != len(fn.Params) {
			want = fmt.Sprintf("%d to %d", required, len(fn.Params))
		}
		return fmt.Sprintf("%s takes %s argument(s), and `%s` passes %d", fn.Signature(), want, ast.FormatExpr(x), n)
	}
	for i, a := range x.Args {
		_, isLambda := ast.Unparen(a).(*ast.LambdaExpr)
		switch wantLambda := fn.Params[i].Type == functions.TypeLambda; {
		case wantLambda && !isLambda:
			return fmt.Sprintf("argument %d of %s is a lambda, as in x => ..., and `%s` is not one", i+1, fn.Signature(), ast.FormatExpr(a))
		case !wantLambda && isLambda:
			return fmt.Sprintf("argument %d of %s is a value, not a lambda", i+1, fn.Signature())
		}
	}
	return ""
}

// StatementBodiesRead is the statement-body gates' coverage: "<file> <name>"
// for every logic and automation in files whose body they read. A body the
// parser refuses is not read, so a caller that
// knows which statement bodies the corpus holds compares the two -- a body the
// gates could not read then cannot pass for one they found clean.
func StatementBodiesRead(files []SourceFile) []string {
	var out []string
	eachStatementBody(files, func(f SourceFile, _, name string, _ int, _ *ast.AutomationDef) {
		out = append(out, f.Path+" "+name)
	})
	return out
}

// eachStatementBody calls visit with every logic and automation in files
// whose body is written in statements, and the line its declaration's text
// starts on. Each declaration is parsed alone, from the first line of its
// annotation preamble to its closing brace, so an automation's header
// lambdas -- @filter and @loop's until -- are read with its statements.
//
// Where each declaration's text starts and ends is the file's, found once per
// process (sourceFacts.statementDeclarations); the parse is per scan, so no
// two scans share an AST.
func eachStatementBody(files []SourceFile, visit func(f SourceFile, kind, name string, startLine int, def *ast.AutomationDef)) {
	for _, f := range files {
		for _, d := range factsOf(f.Content).statementDeclarations() {
			pf, err := languageParser.ParseFile(d.text)
			if err != nil {
				continue // a refusal the loader reports
			}
			for _, def := range pf.Definitions {
				fn, ok := def.(*ast.FunctionDef)
				if !ok || fn.Name != d.name {
					continue
				}
				if auto, ok := fn.Body.(*ast.AutomationDef); ok && auto.Body != nil {
					visit(f, d.kind, d.name, d.startLine, auto)
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
