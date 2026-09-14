package compiler

// body_compile.go -- an edition-2026 body lowered to the executor's step list
// (epic memql#5370, task memql#5371; D12 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The steps come out in SOURCE ORDER and nothing sorts them. The legacy
// compile ran Kahn's algorithm over the references it could see and emitted
// the result, so the order a body ran in was a function of which references
// the extractor happened to recognise. A statement body is checked instead
// (CheckBody refuses a read of a later name), so source order is already an
// order in which every name is bound before it is read.
//
// What each statement becomes:
//
//	x := <kind> f(...)      function (query mutation logic builtin), automation
//	                        or action step; `binds: "x"`
//	x := <expression>       expression step
//	<kind> f(...)           the call's step, no binds
//	if / else if / else     FLATTENED: each statement inside carries the
//	                        conjunction of the conditions that lead to it
//	switch                  FLATTENED the same way: a case is `s == l1 || s == l2`,
//	                        the default the negation of every case
//	for x in s if f { }     forEach step, its body in `do`
//	parallel { branch ... } parallel step, each branch a block step
//	publish "t" { ... }     event step
//	return <expression>     return step
//	return <call>           the call's step, `returns: true`
//
// `binds` is separate from `id`. The id names the step in the run record and
// the journal and is unique within its list; the name is what the statement
// binds, and two sibling branches may bind the same one.
//
// Expressions are written in the encoding epic memql#5363 fixed: a field that
// is always an expression (condition, forEach source and filter, expression,
// return value) holds canonical v1 source, printed by ast.FormatExpr; a value
// inside an argument or payload map is a literal when it is one and
// {"$expr": "<source>"} otherwise, so a string can never be mistaken for a
// reference.

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/ast"
)

// CompileBody checks a body (CheckBody) and lowers it to the executor's step
// list, in source order. Any problem refuses the whole body: the steps are nil
// and the problems are returned.
func CompileBody(kind, name string, args []string, body *ast.Body) ([]map[string]any, []BodyProblem) {
	if ps := CheckBody(kind, name, args, body); len(ps) > 0 {
		return nil, ps
	}
	if body == nil {
		return []map[string]any{}, nil
	}
	return compileStatementList(body.Statements), nil
}

// flatStatement is a statement with the conditions of the once-blocks around
// it, which the flattening turns into the step's own condition.
type flatStatement struct {
	stmt  ast.BodyStatement
	conds []ast.ExpressionNode
}

// compileStatementList lowers one list -- a body, a loop's body, a parallel
// branch -- into steps with ids unique within it.
func compileStatementList(stmts []ast.BodyStatement) []map[string]any {
	var flat []flatStatement
	flattenOnceBlocks(stmts, nil, &flat)
	ids := assignStepIDs(flat)
	out := make([]map[string]any, 0, len(flat))
	for i, f := range flat {
		step := compileStatement(f.stmt)
		step["id"] = ids[i]
		if c := conjunction(f.conds); c != nil {
			step["condition"] = ast.FormatExpr(c)
		}
		out = append(out, step)
	}
	return out
}

// flattenOnceBlocks lifts the statements of every if branch and switch case
// into the enclosing list, each carrying the conditions that lead to it. A
// loop or a parallel keeps its block: it is one step whose children are its
// own list.
func flattenOnceBlocks(stmts []ast.BodyStatement, conds []ast.ExpressionNode, out *[]flatStatement) {
	for _, s := range stmts {
		switch t := s.(type) {
		case nil:
			continue
		case *ast.IfStatement:
			var earlier []ast.ExpressionNode
			for _, b := range t.Branches {
				bc := withConds(conds, negations(earlier)...)
				if b.Cond != nil {
					bc = append(bc, b.Cond)
				}
				flattenOnceBlocks(b.Body, bc, out)
				if b.Cond != nil {
					earlier = append(earlier, b.Cond)
				}
			}
		case *ast.SwitchStatement:
			// Labels are unique (the parser refuses a repeat), so at most one
			// case matches and a case needs no negation of the others. The
			// default runs when none did, wherever it is written.
			var matches []ast.ExpressionNode
			for _, c := range t.Cases {
				if !c.Default {
					matches = append(matches, caseMatch(t.Subject, c.Labels))
				}
			}
			for _, c := range t.Cases {
				if c.Default {
					flattenOnceBlocks(c.Body, withConds(conds, negations(matches)...), out)
					continue
				}
				flattenOnceBlocks(c.Body, withConds(conds, caseMatch(t.Subject, c.Labels)), out)
			}
		default:
			*out = append(*out, flatStatement{stmt: s, conds: conds})
		}
	}
}

// withConds returns conds followed by more, in a new slice: sibling branches
// must not share a backing array.
func withConds(conds []ast.ExpressionNode, more ...ast.ExpressionNode) []ast.ExpressionNode {
	out := make([]ast.ExpressionNode, 0, len(conds)+len(more))
	out = append(out, conds...)
	return append(out, more...)
}

func negations(conds []ast.ExpressionNode) []ast.ExpressionNode {
	out := make([]ast.ExpressionNode, 0, len(conds))
	for _, c := range conds {
		out = append(out, &ast.UnaryExpr{Op: "!", Operand: c})
	}
	return out
}

// caseMatch is `subject == l1 || subject == l2 ...`. The printer adds the
// parentheses a subject needs, so the condition means what the switch meant.
func caseMatch(subject ast.ExpressionNode, labels []ast.ExpressionNode) ast.ExpressionNode {
	var m ast.ExpressionNode
	for _, l := range labels {
		eq := &ast.BinaryExpr{Op: "==", Left: subject, Right: l}
		if m == nil {
			m = eq
			continue
		}
		m = &ast.BinaryExpr{Op: "||", Left: m, Right: eq}
	}
	return m
}

// conjunction ANDs conds, left to right; nil when there are none.
func conjunction(conds []ast.ExpressionNode) ast.ExpressionNode {
	var c ast.ExpressionNode
	for _, x := range conds {
		if c == nil {
			c = x
			continue
		}
		c = &ast.BinaryExpr{Op: "&&", Left: c, Right: x}
	}
	return c
}

// assignStepIDs gives each step of one list its id. Named statements claim
// their names first, in source order (`x`, then `x#2` for a sibling branch's
// rebinding); every other statement then takes its callee's name, `for_<var>`,
// `parallel`, `publish` or `return`, numbered the same way. An id therefore
// changes only when a statement of the same list with the same base changes.
func assignStepIDs(flat []flatStatement) []string {
	claimed := map[string]bool{}
	ids := make([]string, len(flat))
	for i, f := range flat {
		if a, ok := f.stmt.(*ast.AssignStatement); ok {
			ids[i] = claimStepID(a.Name, claimed)
		}
	}
	for i, f := range flat {
		if ids[i] == "" {
			ids[i] = claimStepID(stepIDBase(f.stmt), claimed)
		}
	}
	return ids
}

func claimStepID(base string, claimed map[string]bool) string {
	if !claimed[base] {
		claimed[base] = true
		return base
	}
	for n := 2; ; n++ {
		id := fmt.Sprintf("%s#%d", base, n)
		if !claimed[id] {
			claimed[id] = true
			return id
		}
	}
}

func stepIDBase(s ast.BodyStatement) string {
	switch t := s.(type) {
	case *ast.CallStatement:
		return t.Call.Name
	case *ast.ReturnStatement:
		if t.Call != nil {
			return t.Call.Name
		}
		return "return"
	case *ast.ForStatement:
		return "for_" + t.Var
	case *ast.ParallelStatement:
		return "parallel"
	case *ast.PublishStatement:
		return "publish"
	}
	return ast.StatementKind(s)
}

// compileStatement lowers one statement that flattening left standing.
func compileStatement(s ast.BodyStatement) map[string]any {
	switch t := s.(type) {
	case *ast.AssignStatement:
		if t.Call != nil {
			step := callStep(t.Call, t.Mods)
			step["binds"] = t.Name
			return step
		}
		return map[string]any{
			"type":       "expression",
			"expression": ast.FormatExpr(t.Value),
			"binds":      t.Name,
		}
	case *ast.CallStatement:
		return callStep(t.Call, t.Mods)
	case *ast.ForStatement:
		fe := map[string]any{
			"source": ast.FormatExpr(t.Source),
			"as":     t.Var,
			"do":     compileStatementList(t.Body),
		}
		if t.Filter != nil {
			fe["filter"] = ast.FormatExpr(t.Filter)
		}
		step := map[string]any{"type": "forEach", "forEach": fe}
		addOnError(step, t.Mods)
		return step
	case *ast.ParallelStatement:
		wait := t.Wait
		if wait == "" {
			wait = "all"
		}
		branches := make([]map[string]any, 0, len(t.Branches))
		for _, b := range t.Branches {
			branches = append(branches, map[string]any{
				"id":    b.Label,
				"type":  "block",
				"block": map[string]any{"steps": compileStatementList(b.Body)},
			})
		}
		// A failed branch stops the others: the statement form has no way to
		// say otherwise (the legacy `failFast: false` has no spelling), and
		// `on error continue` on the parallel is how a body says "go on".
		step := map[string]any{
			"type":     "parallel",
			"parallel": map[string]any{"wait": wait, "failFast": true, "branches": branches},
		}
		addOnError(step, t.Mods)
		return step
	case *ast.PublishStatement:
		ev := map[string]any{"topic": t.Topic}
		if t.Payload != nil {
			ev["payload"] = encodeValueLeaf(t.Payload)
		}
		return map[string]any{"type": "event", "event": ev}
	case *ast.ReturnStatement:
		if t.Call != nil {
			step := callStep(t.Call, t.Mods)
			step["returns"] = true
			return step
		}
		ret := map[string]any{}
		if t.Value != nil {
			ret["value"] = ast.FormatExpr(t.Value)
		}
		return map[string]any{"type": "return", "return": ret}
	}
	// Unreachable while flattenOnceBlocks lifts every if and switch and the
	// statement set is closed (TestCompileBodyCoversEveryStatementKind).
	return map[string]any{"type": ast.StatementKind(s)}
}

// callStep lowers a construct call and its trailing clauses.
func callStep(c *ast.ConstructCall, mods ast.StatementMods) map[string]any {
	var step map[string]any
	args := encodeNamedArgs(c.Args)
	switch c.Kind {
	case "automation":
		cfg := map[string]any{"name": c.Name}
		if args != nil {
			cfg["args"] = args
		}
		step = map[string]any{"type": "automation", "automation": cfg}
	case "action":
		cfg := map[string]any{"ref": c.Name}
		if args != nil {
			cfg["args"] = args
		}
		if c.Surface != "" {
			cfg["surface"] = c.Surface
		}
		step = map[string]any{"type": "action", "action": cfg}
	default:
		cfg := map[string]any{"name": c.Name, "kind": c.Kind}
		if args != nil {
			cfg["args"] = args
		}
		step = map[string]any{"type": "function", "function": cfg}
	}
	if mods.Retry > 0 {
		step["retryCount"] = mods.Retry
	}
	addOnError(step, mods)
	return step
}

func addOnError(step map[string]any, mods ast.StatementMods) {
	if mods.OnError != "" {
		step["onError"] = mods.OnError
	}
}

func encodeNamedArgs(args []ast.NamedArg) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for _, a := range args {
		out[a.Name] = encodeValueLeaf(a.Value)
	}
	return out
}

// encodeValueLeaf writes a value that sits inside an argument or payload map:
// a literal as itself, a map or list literal as the structure with each
// element encoded, and any other expression as {"$expr": "<source>"}.
func encodeValueLeaf(e ast.ExpressionNode) any {
	switch v := ast.Unparen(e).(type) {
	case *ast.LiteralExpr:
		return v.Value
	case *ast.NilExpr:
		return nil
	case *ast.MapExpr:
		out := make(map[string]any, len(v.Entries))
		for _, en := range v.Entries {
			out[en.Key] = encodeValueLeaf(en.Value)
		}
		return out
	case *ast.ListExpr:
		out := make([]any, 0, len(v.Elems))
		for _, el := range v.Elems {
			out = append(out, encodeValueLeaf(el))
		}
		return out
	}
	return map[string]any{"$expr": ast.FormatExpr(e)}
}
