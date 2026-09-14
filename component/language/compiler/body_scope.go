package compiler

// body_scope.go -- names and scope in an edition-2026 body (epic memql#5370,
// task memql#5371; D12 and D14 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A body's statements run in source order, so a name is readable from the
// statement after the one that binds it and never before. That check is what
// replaces the topological sort the compiler used to run over step references,
// which read a reference to a later step as an instruction to reorder. Now it
// is a load error naming both lines.
//
// The rules, from the record and the owner's answers (2026-09-13):
//
//   - A bare name is a statement name, a loop variable, a lambda parameter or
//     a reserved root. Arguments are read args.x, in both keywords.
//   - A block that runs at most once -- an if/else branch, a switch case --
//     shares the enclosing scope: a name bound there is readable after the if
//     or switch, and absent when its branch did not run. The branches of ONE
//     if/else chain or ONE switch may bind the same name; whichever runs binds
//     it. Reading a name bound in another branch of the same chain is refused,
//     because that branch cannot have run.
//   - A loop body and a parallel branch have their own scope: a name bound
//     inside exists only inside, and may not shadow a name of an enclosing
//     scope.
//   - A name is bound once per scope, apart from the sibling-branch rule.
//
// and the construct rules of D14: a logic may not publish or call an
// automation or an action, a logic ends with `return`, and a parallel branch
// cannot return.
//
// # How it decides
//
// One walk records every binding and every read with a tick (its place in
// source order, a statement's reads before its own binding), the scope it sits
// in, and its branch path: the (chain, branch) pairs of the once-blocks around
// it. Resolution then needs no second notion of order:
//
//   - a binding is visible to a read when its scope encloses the read's, its
//     tick is earlier, and the two paths do not part inside one chain (which
//     would put them in branches that exclude each other);
//   - two bindings in one scope conflict unless their paths part inside one
//     chain.
//
// A read that resolves to nothing is classified by the bindings it did NOT
// see -- a later one is a forward reference, one in an excluded branch or in
// an inner scope says so -- because the message is the author's way out.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// The refusal codes of the scope checker: the stable half of each problem,
// which the conformance corpus and Sense key on rather than on the wording.
const (
	codeBodyForwardReference = "body_forward_reference"
	codeBodyUnknownName      = "body_unknown_name"
	codeBodyDuplicateName    = "body_duplicate_name"
	codeBodyShadowedName     = "body_shadowed_name"
	codeBodyReservedName     = "body_reserved_name"
	codeBodyPublishInLogic   = "body_publish_in_logic"
	codeBodyCallNotInLogic   = "body_call_not_in_logic"
	codeBodyLogicReturn      = "body_logic_return"
	codeBodyReturnInParallel = "body_return_in_parallel"
)

// BodyProblemCodes lists every code CheckBody can return.
func BodyProblemCodes() []string {
	return []string{
		codeBodyForwardReference,
		codeBodyUnknownName,
		codeBodyDuplicateName,
		codeBodyShadowedName,
		codeBodyReservedName,
		codeBodyPublishInLogic,
		codeBodyCallNotInLogic,
		codeBodyLogicReturn,
		codeBodyReturnInParallel,
	}
}

// BodyProblem is a load-time refusal of a body.
type BodyProblem struct {
	Code      string
	Message   string
	Construct string // "logic requestRouteStatus"
	Line, Col int
}

// Error names the construct, the position, the message and, last, the code in
// square brackets (D24).
func (p BodyProblem) Error() string {
	if p.Line == 0 {
		return fmt.Sprintf("%s: %s [%s]", p.Construct, p.Message, p.Code)
	}
	return fmt.Sprintf("%s, line %d:%d: %s [%s]", p.Construct, p.Line, p.Col, p.Message, p.Code)
}

// BodyProblems is every problem of one body, as one error: each on its own
// line, in source order.
type BodyProblems []BodyProblem

func (ps BodyProblems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.Error()
	}
	return strings.Join(lines, "\n")
}

// bodyReservedNames may not name a statement or a loop variable. Every one but
// steps is readable as a root somewhere; steps is the retired step-result
// namespace, which the parser refuses to read.
var bodyReservedNames = map[string]bool{
	"args": true, "actor": true, "event": true, "now": true,
	"config": true, "partition": true, "trace": true, "steps": true,
}

// IsBodyRoot reports whether a bare read of name is a reserved root in a body
// of this kind ("logic" or "automation") -- the roots Sense offers there.
func IsBodyRoot(kind, name string) bool { return isRoot(kind, name) }

// isRoot reports whether a bare read of name is a reserved root in a body of
// this kind. `event` is an automation's trigger; a logic has none of its own
// and reads what its caller passes, args.event.
func isRoot(kind, name string) bool {
	switch name {
	case "args", "actor", "now", "config", "partition", "trace":
		return true
	case "event":
		return kind == "automation"
	}
	return false
}

// CheckBody enforces the scope rules and the construct rules of D14 on one
// body. kind is "logic" or "automation"; args are the declared args field
// names, which only shape a message. It returns every problem, in source order.
func CheckBody(kind, name string, args []string, body *ast.Body) []BodyProblem {
	w := newScopeWalk(kind, name)
	var stmts []ast.BodyStatement
	if body != nil {
		stmts = body.Statements
	}
	w.statements(stmts, w.root, nil, nil, false)
	declared := map[string]bool{}
	for _, a := range args {
		declared[a] = true
	}
	w.checkBindings()
	w.resolve(declared)
	if kind == "logic" {
		w.checkLogicReturn(body)
	}
	sort.SliceStable(w.problems, func(i, j int) bool {
		a, b := w.problems[i], w.problems[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	return w.problems
}

// BodyNames returns every name a body binds -- statement names and loop
// variables, at any depth -- with the lines that bind it, ascending. It is the
// scope checker's own table, exported for Sense.
func BodyNames(body *ast.Body) map[string][]int {
	out := map[string][]int{}
	if body == nil {
		return out
	}
	w := newScopeWalk("", "")
	w.statements(body.Statements, w.root, nil, nil, false)
	for _, b := range w.bindings {
		out[b.name] = append(out[b.name], b.line)
	}
	for n := range out {
		sort.Ints(out[n])
	}
	return out
}

type scopeKind int

const (
	scopeBody scopeKind = iota
	scopeLoop
	scopeParallel
)

// bodyScope is one scope: the body's own, a loop body's or a parallel
// branch's. Once-blocks open none; they are branch paths within a scope.
type bodyScope struct {
	parent *bodyScope
	kind   scopeKind
}

func (s *bodyScope) encloses(t *bodyScope) bool {
	for ; t != nil; t = t.parent {
		if t == s {
			return true
		}
	}
	return false
}

// onceStep is one once-block around a statement: which chain (an if/else
// chain or a switch) and which of its branches.
type onceStep struct{ chain, branch int }

// partingChain returns the chain inside which two branch paths part, if they
// do: the first place they differ is two branches of one chain. Paths that
// part between different chains, or where one is a prefix of the other, are
// not exclusive -- the first chain ended before the second began.
func partingChain(a, b []onceStep) (int, bool) {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		if a[i].chain == b[i].chain {
			return a[i].chain, true
		}
		return 0, false
	}
	return 0, false
}

func exclusive(a, b []onceStep) bool {
	_, ok := partingChain(a, b)
	return ok
}

// ancestor is one statement on the way down to a binding or a read: the block
// (statement list) it sits in, a unique id, and its line.
type ancestor struct {
	block, stmt, line int
}

type bodyBinding struct {
	name      string
	line, col int
	tick      int
	stmt      int
	scope     *bodyScope
	path      []onceStep
	ancestry  []ancestor
}

type bodyRead struct {
	name      string
	line, col int
	tick      int
	stmt      int
	label     string // the reading statement, as a message quotes it
	stmtLine  int
	scope     *bodyScope
	path      []onceStep
	ancestry  []ancestor
}

type scopeWalk struct {
	kind, construct string
	root            *bodyScope
	bindings        []*bodyBinding
	reads           []*bodyRead
	problems        []BodyProblem
	tick            int
	blocks          int
	stmts           int
	chainKinds      []string
	scopeLines      map[*bodyScope]int
}

func newScopeWalk(kind, name string) *scopeWalk {
	return &scopeWalk{
		kind:       kind,
		construct:  strings.TrimSpace(kind + " " + name),
		root:       &bodyScope{kind: scopeBody},
		scopeLines: map[*bodyScope]int{},
	}
}

func (w *scopeWalk) problem(code string, line, col int, format string, a ...any) {
	w.problems = append(w.problems, BodyProblem{
		Code:      code,
		Message:   fmt.Sprintf(format, a...),
		Construct: w.construct,
		Line:      line,
		Col:       col,
	})
}

func (w *scopeWalk) nextTick() int {
	w.tick++
	return w.tick
}

func appendPath(path []onceStep, s onceStep) []onceStep {
	out := make([]onceStep, len(path)+1)
	copy(out, path)
	out[len(path)] = s
	return out
}

// statements walks one block. anc is the ancestry of the block's statements'
// parent; each statement adds itself.
func (w *scopeWalk) statements(stmts []ast.BodyStatement, sc *bodyScope, path []onceStep, anc []ancestor, inParallel bool) {
	w.blocks++
	block := w.blocks
	for _, s := range stmts {
		if s == nil {
			continue
		}
		w.stmts++
		me := ancestor{block: block, stmt: w.stmts, line: s.StatementSpan().Line}
		sanc := append(append([]ancestor(nil), anc...), me)
		w.statement(s, sc, path, sanc, inParallel)
	}
}

// statement records one statement's reads and bindings, then walks its
// blocks. The order of the calls below IS the order a statement reads and
// binds in: its expressions first, its own name after.
func (w *scopeWalk) statement(s ast.BodyStatement, sc *bodyScope, path []onceStep, anc []ancestor, inParallel bool) {
	self := anc[len(anc)-1]
	label := statementLabel(s)
	read := func(e ast.ExpressionNode, in *bodyScope, p []onceStep) {
		w.expression(e, in, p, anc, self.stmt, label, self.line)
	}
	readCall := func(c *ast.ConstructCall) {
		if c == nil {
			return
		}
		w.callRule(c)
		for _, a := range c.Args {
			read(a.Value, sc, path)
		}
	}
	switch t := s.(type) {
	case *ast.AssignStatement:
		readCall(t.Call)
		read(t.Value, sc, path)
		w.bind(t.Name, spanOr(t.NameSpan, t.Span), self.stmt, sc, path, anc)
	case *ast.CallStatement:
		readCall(t.Call)
	case *ast.IfStatement:
		chain := w.newChain("if")
		for i, b := range t.Branches {
			bp := appendPath(path, onceStep{chain, i})
			// A later branch's condition is evaluated only when the earlier
			// branches did not run, so it reads from inside its own branch.
			read(b.Cond, sc, bp)
			w.statements(b.Body, sc, bp, anc, inParallel)
		}
	case *ast.ForStatement:
		read(t.Source, sc, path)
		loop := &bodyScope{parent: sc, kind: scopeLoop}
		w.scopeLines[loop] = self.line
		w.bind(t.Var, spanOr(t.VarSpan, t.Span), self.stmt, loop, path, anc)
		read(t.Filter, loop, path)
		w.statements(t.Body, loop, path, anc, inParallel)
	case *ast.SwitchStatement:
		read(t.Subject, sc, path)
		chain := w.newChain("switch")
		for i, c := range t.Cases {
			for _, l := range c.Labels {
				read(l, sc, path)
			}
			w.statements(c.Body, sc, appendPath(path, onceStep{chain, i}), anc, inParallel)
		}
	case *ast.ParallelStatement:
		for _, b := range t.Branches {
			branch := &bodyScope{parent: sc, kind: scopeParallel}
			w.scopeLines[branch] = spanOr(b.Span, t.Span).Line
			w.statements(b.Body, branch, path, anc, true)
		}
	case *ast.PublishStatement:
		if w.kind == "logic" {
			sp := t.Span
			w.problem(codeBodyPublishInLogic, sp.Line, sp.Col,
				"a logic may not publish: move `publish %s` into the automation that calls %s, or declare %s as an automation",
				ast.QuoteString(t.Topic), w.constructName(), w.constructName())
		}
		if t.Payload != nil {
			read(t.Payload, sc, path)
		}
	case *ast.ReturnStatement:
		if inParallel {
			sp := t.Span
			w.problem(codeBodyReturnInParallel, sp.Line, sp.Col,
				"a parallel branch cannot return: bind a name and return after the parallel")
		}
		readCall(t.Call)
		read(t.Value, sc, path)
	}
}

// LogicMayCall reports whether a logic may call a construct of kind: every
// kind a statement calls but the two D14 keeps out of a logic, automation and
// action, which belong in an automation. Sense offers what it admits.
func LogicMayCall(kind string) bool {
	return kind != "automation" && kind != "action"
}

// callRule refuses, in a logic, the call kinds LogicMayCall does not admit.
func (w *scopeWalk) callRule(c *ast.ConstructCall) {
	if w.kind != "logic" || LogicMayCall(c.Kind) {
		return
	}
	w.problem(codeBodyCallNotInLogic, c.Span.Line, c.Span.Col,
		"a logic calls queries, mutations, builtins and other logic: `%s %s(...)` belongs in an automation", c.Kind, c.Name)
}

func (w *scopeWalk) constructName() string {
	if i := strings.IndexByte(w.construct, ' '); i >= 0 {
		return w.construct[i+1:]
	}
	return w.construct
}

func (w *scopeWalk) newChain(kind string) int {
	w.chainKinds = append(w.chainKinds, kind)
	return len(w.chainKinds) - 1
}

func (w *scopeWalk) bind(name string, sp ast.Span, stmt int, sc *bodyScope, path []onceStep, anc []ancestor) {
	if name == "" {
		return
	}
	w.bindings = append(w.bindings, &bodyBinding{
		name: name, line: sp.Line, col: sp.Col, tick: w.nextTick(), stmt: stmt,
		scope: sc, path: path, ancestry: anc,
	})
}

// expression records every read in e. A lambda's parameters are in scope in
// its body and shadow anything outside it; a function's name is a callee, not
// a read (CallExpr.Name is a string, never an IdentExpr).
func (w *scopeWalk) expression(e ast.ExpressionNode, sc *bodyScope, path []onceStep, anc []ancestor, stmt int, label string, stmtLine int) {
	if e == nil {
		return
	}
	var walk func(n ast.ExpressionNode, params map[string]bool)
	walk = func(n ast.ExpressionNode, params map[string]bool) {
		ast.WalkV1(n, func(x ast.ExpressionNode) bool {
			switch v := x.(type) {
			case *ast.LambdaExpr:
				inner := map[string]bool{}
				for p := range params {
					inner[p] = true
				}
				for _, p := range v.Params {
					inner[p] = true
				}
				walk(v.Body, inner)
				return false
			case *ast.IdentExpr:
				if !params[v.Name] {
					w.reads = append(w.reads, &bodyRead{
						name: v.Name, line: v.Span.Line, col: v.Span.Col, tick: w.nextTick(),
						stmt: stmt, label: label, stmtLine: stmtLine, scope: sc, path: path, ancestry: anc,
					})
				}
			}
			return true
		})
	}
	walk(e, nil)
}

// checkBindings refuses a reserved name, a second binding in one scope, and a
// loop or parallel-branch name that shadows an enclosing one.
func (w *scopeWalk) checkBindings() {
	for i, b := range w.bindings {
		if bodyReservedNames[b.name] {
			w.problem(codeBodyReservedName, b.line, b.col,
				"`%s` is a reserved root and cannot name a statement or a loop variable", b.name)
			continue
		}
		for _, p := range w.bindings[:i] {
			if p.name != b.name || exclusive(p.path, b.path) {
				continue
			}
			if p.scope == b.scope {
				w.problem(codeBodyDuplicateName, b.line, b.col,
					"`%s` is already bound on line %d: a name is bound once, apart from the branches of one if/else chain or switch", b.name, p.line)
				break
			}
			if b.scope.kind != scopeBody && p.scope.encloses(b.scope) {
				w.problem(codeBodyShadowedName, b.line, b.col,
					"`%s` on line %d shadows `%s` bound on line %d: pick another name", b.name, b.line, b.name, p.line)
				break
			}
		}
	}
}

// resolve checks every read against the bindings it can see.
func (w *scopeWalk) resolve(declared map[string]bool) {
	byName := map[string][]*bodyBinding{}
	for _, b := range w.bindings {
		byName[b.name] = append(byName[b.name], b)
	}
	for _, r := range w.reads {
		if isRoot(w.kind, r.name) {
			continue
		}
		seen := false
		for _, b := range byName[r.name] {
			if b.tick < r.tick && b.scope.encloses(r.scope) && !exclusive(b.path, r.path) {
				seen = true
				break
			}
		}
		if !seen {
			w.unresolved(r, byName[r.name], declared)
		}
	}
}

// unresolved reports a read that sees no binding, classified by the bindings
// it did not see.
func (w *scopeWalk) unresolved(r *bodyRead, bs []*bodyBinding, declared map[string]bool) {
	for _, b := range bs {
		if b.stmt == r.stmt {
			w.problem(codeBodyUnknownName, r.line, r.col,
				"`%s` is not a statement name, a loop variable or a root here: a statement cannot read the name it binds", r.name)
			return
		}
	}
	for _, b := range bs {
		if b.tick > r.tick && b.scope.encloses(r.scope) && !exclusive(b.path, r.path) {
			move, above := moveLines(b.ancestry, r.ancestry, b.line, r.stmtLine)
			w.problem(codeBodyForwardReference, r.line, r.col,
				"%s reads `%s`, which is bound on line %d, after it: move line %d above line %d", r.label, r.name, b.line, move, above)
			return
		}
	}
	msg := fmt.Sprintf("`%s` is not a statement name, a loop variable or a root here", r.name)
	for _, b := range bs {
		if !b.scope.encloses(r.scope) {
			continue
		}
		if chain, ok := partingChain(b.path, r.path); ok {
			w.problem(codeBodyUnknownName, r.line, r.col,
				"%s; %s is bound in another branch of this %s on line %d, which cannot have run", msg, r.name, w.chainKinds[chain], b.line)
			return
		}
	}
	for _, b := range bs {
		if b.scope.encloses(r.scope) {
			continue
		}
		inside := "loop"
		if b.scope.kind == scopeParallel {
			inside = "parallel branch"
		}
		w.problem(codeBodyUnknownName, r.line, r.col,
			"%s; %s is bound inside the %s on line %d and exists only inside it", msg, r.name, inside, w.scopeOpener(b.scope, b.line))
		return
	}
	switch {
	case declared[r.name]:
		msg += fmt.Sprintf("; write args.%s", r.name)
	case r.name == "event":
		msg += "; a logic has no trigger of its own: declare `event` in its args and read args.event, which its caller passes"
	case r.name == "steps":
		msg += "; `steps.<id>` is retired: a statement's name is its value"
	}
	w.problem(codeBodyUnknownName, r.line, r.col, "%s", msg)
}

// scopeOpener is the line of the for or branch that opened an inner scope,
// falling back to the binding's own line.
func (w *scopeWalk) scopeOpener(sc *bodyScope, fallback int) int {
	if l := w.scopeLines[sc]; l > 0 {
		return l
	}
	return fallback
}

// moveLines answers "move line <m> above line <n>" for a read of a name bound
// later: in the innermost block holding both, the statement that contains the
// binding moves above the statement that contains the read. Moving the binding
// line alone could carry it into or out of a branch and change what it means.
func moveLines(bind, read []ancestor, bindLine, readLine int) (int, int) {
	for i := 0; i < len(bind) && i < len(read); i++ {
		if bind[i].block != read[i].block {
			break
		}
		if bind[i].stmt != read[i].stmt {
			return bind[i].line, read[i].line
		}
	}
	return bindLine, readLine
}

// checkLogicReturn requires a logic's last top-level statement to be a return.
func (w *scopeWalk) checkLogicReturn(body *ast.Body) {
	var stmts []ast.BodyStatement
	if body != nil {
		stmts = body.Statements
	}
	if len(stmts) > 0 {
		if _, ok := stmts[len(stmts)-1].(*ast.ReturnStatement); ok {
			return
		}
	}
	var sp ast.Span
	switch {
	case len(stmts) > 0:
		sp = stmts[len(stmts)-1].StatementSpan()
	case body != nil:
		sp = body.Span
	}
	w.problem(codeBodyLogicReturn, sp.Line, sp.Col,
		"a logic ends with `return <value>`: its last statement, outside any block, is the return")
}

// statementLabel names a statement the way a message quotes it.
func statementLabel(s ast.BodyStatement) string {
	switch t := s.(type) {
	case *ast.AssignStatement:
		return "`" + t.Name + "`"
	case *ast.CallStatement:
		if t.Call != nil {
			return "`" + t.Call.Kind + " " + t.Call.Name + "`"
		}
	case *ast.ForStatement:
		return "`for " + t.Var + "`"
	case *ast.PublishStatement:
		return "`publish " + ast.QuoteString(t.Topic) + "`"
	}
	return "`" + ast.StatementKind(s) + "`"
}

func spanOr(sp, fallback ast.Span) ast.Span {
	if sp.IsZero() {
		return fallback
	}
	return sp
}
