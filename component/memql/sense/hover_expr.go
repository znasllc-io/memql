package sense

// hover_expr.go is hover over the v1 expression vocabulary (memql#5365): the
// catalog's functions and methods, the operators, and the spellings the
// language retired. Every card has the same restrained shape, plain text with
// the form the author writes first:
//
//	```memql
//	lower(value string) string
//	```
//	Returns value with its letters lowercased.
//
//	Runs in process before the query, on values that do not read the row.
//
//	Not allowed on the row in a query filter: `lower` runs in process. ...
//
// The where-it-runs line is chosen by the cursor's position; the legality line
// appears only when the position refuses or restricts the item, and names the
// fix with a code span; a function that replaces retired spellings says which.
// Sentence case, no labels, no separators.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// migrator names the rewrite that migrates every retired expression spelling.
const migrator = "memqlmigrate --rewrite=expressions"

// callSite is what a hover card knows about the call under the cursor, so the
// legality line can name the author's own field instead of a placeholder.
type callSite struct {
	// rowField is the row member the call's first argument reads (`email` in
	// `lower(row.email)`), or "".
	rowField string
	// receiver is a method call's receiver path (`row.tags` in
	// `row.tags.where(...)`), or "".
	receiver string
	// rowFree reports that the call demonstrably reads nothing from the row:
	// it is complete, and neither its receiver nor its arguments name a lambda
	// parameter in scope. Such a call is a plan constant -- legal where the
	// position admits the entry only on plan constants -- so its card carries
	// no legality line.
	rowFree bool
}

// expressionHover answers hover for an operator, a retired spelling, or a
// catalog function or method, and reports whether the token under the cursor
// was one.
func (s *Service) expressionHover(source string, line, col int) (*HoverResult, bool) {
	toks := lexTokens(source)
	idx := tokenIndexAt(toks, line, col)
	if idx < 0 {
		return nil, false
	}
	t := toks[idx]
	ctx := analyzeCursorContext(source, line, col)
	ctx.FilePath = ""
	rng := Range{Start: Position{Line: t.Line, Column: t.Column}, End: Position{Line: t.EndLine, Column: t.EndCol}}
	card := func(contents string) (*HoverResult, bool) {
		return &HoverResult{Contents: contents, Range: rng}, true
	}

	if op, ok := operatorAt(toks, idx, ctx); ok {
		unary := op.Name == "not" || op.Name == "negate"
		return card(operatorCard(op, ctx.Position, operandsRowFree(toks, idx, ctx.Params, unary)))
	}
	if spelling, replacement, ok := retiredAt(toks, idx, col); ok {
		return card(retiredCard(spelling, replacement))
	}
	if f, site, ok := s.catalogEntryAt(toks, idx, line, col, ctx); ok {
		a := tiers.Admitted
		if ctx.Position != "" {
			a = tiers.FunctionAdmission(ctx.Position, f.Key())
		}
		return card(catalogCard(f, ctx.Position, a, ctx.Param, site))
	}
	return nil, false
}

// tokenIndexAt returns the index of the token under a 1-based cursor, or -1.
// A column on the boundary between two tokens belongs to the earlier one, as
// in tokenAtPosition.
func tokenIndexAt(toks []parser.Token, line, col int) int {
	for i, t := range toks {
		if t.Line == line && col >= t.Column && col <= t.EndCol && t.EndLine == t.Line {
			return i
		}
	}
	return -1
}

// ---- Operators ----

// operatorAt returns the operator the token at idx is, if it is one of the
// v1 operators in the position it appears in. The spellings a v1 operator
// shares with something else are told apart here: `in` in a forEach header is
// the loop keyword, the terse automation's `=>` names its target, a map key's
// `:` is not the ternary's, and the required sigil `string!` sits outside any
// expression position.
func operatorAt(toks []parser.Token, idx int, ctx CursorContext) (functions.Operator, bool) {
	t := toks[idx]
	var name string
	switch t.Type {
	case parser.TokenQuestionQuestion:
		name = "coalesce"
	case parser.TokenAmpAmp:
		name = "and"
	case parser.TokenPipePipe:
		name = "or"
	case parser.TokenKeywordStartsWith:
		name = "startsWith"
	case parser.TokenKeywordIn:
		if first := firstWordOfLine(toks, idx); first == "forEach" || first == "for" {
			return functions.Operator{}, false
		}
		name = "in"
	case parser.TokenBang:
		if ctx.Position == "" {
			return functions.Operator{}, false
		}
		name = "not"
	case parser.TokenDotQuestion:
		name = "optionalMember"
	case parser.TokenQuestion:
		name = "ternary"
	case parser.TokenColon:
		if isTernaryColon(toks, idx) {
			name = "ternary"
		}
	case parser.TokenOperator:
		switch t.Literal {
		case "==":
			name = "equal"
		case "!=":
			name = "notEqual"
		case "<":
			name = "less"
		case "<=":
			name = "lessOrEqual"
		case ">":
			name = "greater"
		case ">=":
			name = "greaterOrEqual"
		case "+":
			name = "add"
		case "*":
			name = "multiply"
		case "/":
			name = "divide"
		case "%":
			name = "remainder"
		case "-":
			name = "subtract"
			if idx == 0 || isOperandStart(toks[idx-1]) {
				name = "negate"
			}
		case "=>":
			if isLambdaArrow(toks, idx) {
				name = "lambda"
			}
		}
	}
	if name == "" {
		return functions.Operator{}, false
	}
	for _, op := range functions.Operators() {
		if op.Name == name {
			return op, true
		}
	}
	return functions.Operator{}, false
}

// operatorCard renders an operator's hover card at a position. rowFree reports
// that its operands demonstrably do not read the row, which makes a
// plan-constant-only operator legal where it stands.
func operatorCard(op functions.Operator, pos tiers.Position, rowFree bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "```memql\n%s\n```\n\n%s", op.Form, op.Doc)
	if op.Absence != "" {
		b.WriteString("\n\n" + op.Absence)
	}
	if pos != "" {
		switch tiers.KindAdmission(pos, ast.NodeKind(op.Kind)) {
		case tiers.PlanConstantOnly:
			if rowFree {
				break
			}
			fmt.Fprintf(&b, "\n\nNot allowed on the row in %s: `%s` runs in process. %s", positionPhrase(pos), op.Symbol, operatorFix(op))
		case tiers.Refused:
			fmt.Fprintf(&b, "\n\nNot allowed in %s, which %s.", positionPhrase(pos), literalRule(pos))
		}
	}
	return b.String()
}

// operatorFix is the pushdown spelling of an in-process operator: move it to
// the side of the comparison that does not read the row.
func operatorFix(op functions.Operator) string {
	switch op.Kind {
	case "coalesce":
		return "Coalesce a value that does not read the row: `row.stage == (args.stage ?? \"active\")`."
	case "arithmetic":
		return "Keep the arithmetic on the side that does not read the row: `row.count > args.floor * 2`."
	case "negate":
		return "Negate the side that does not read the row: `row.delta > -args.limit`."
	}
	return "Write it over values that do not read the row."
}

// operandsRowFree reports whether an operator's operands demonstrably do not
// read the row: each is a single token -- a literal, or a name that is not a
// lambda parameter in scope. A bracketed operand is not looked into, and is
// not free.
func operandsRowFree(toks []parser.Token, idx int, params []LambdaParam, unary bool) bool {
	simple := func(t parser.Token) bool {
		switch t.Type {
		case parser.TokenIdentifier:
			return !readsParam(t.Literal, params)
		case parser.TokenString, parser.TokenNumber, parser.TokenKeywordNil:
			return true
		}
		return false
	}
	if idx+1 >= len(toks) || !simple(toks[idx+1]) {
		return false
	}
	return unary || (idx > 0 && simple(toks[idx-1]))
}

// callArgsRowFree reports whether a complete call's arguments demonstrably do
// not read the row: the call closes, and no name inside it is a lambda
// parameter in scope. An unfinished call cannot be told, and is not free.
func callArgsRowFree(toks []parser.Token, open int, params []LambdaParam) bool {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i].Type {
		case parser.TokenParenOpen, parser.TokenBracketOpen, parser.TokenBraceOpen:
			depth++
		case parser.TokenParenClose, parser.TokenBracketClose, parser.TokenBraceClose:
			depth--
			if depth == 0 {
				return true
			}
		case parser.TokenIdentifier:
			if readsParam(toks[i].Literal, params) {
				return false
			}
		}
	}
	return false
}

// readsParam reports whether a (possibly dotted) name starts at a lambda
// parameter in scope -- the row, or a value taken from it.
func readsParam(name string, params []LambdaParam) bool {
	head, _, _ := strings.Cut(name, ".")
	_, ok := paramInScope(params, head)
	return ok
}

// isTernaryColon reports whether the `:` at idx closes a ternary: walking back
// through the brackets it sits in, a `?` is still waiting for its `:`. A map
// key's or a named argument's colon has none.
func isTernaryColon(toks []parser.Token, idx int) bool {
	depth, questions, colons := 0, 0, 0
	for i := idx - 1; i >= 0; i-- {
		switch toks[i].Type {
		case parser.TokenParenClose, parser.TokenBracketClose, parser.TokenBraceClose:
			depth++
		case parser.TokenParenOpen, parser.TokenBracketOpen, parser.TokenBraceOpen:
			depth--
			if depth < 0 {
				return questions > colons
			}
		case parser.TokenQuestion:
			if depth == 0 {
				questions++
			}
		case parser.TokenColon:
			if depth == 0 {
				colons++
			}
		case parser.TokenDefine:
			return questions > colons
		}
	}
	return questions > colons
}

// isLambdaArrow reports whether the `=>` at idx is a lambda's: it follows a
// bare parameter name or a parenthesised list of them. The terse automation
// header's arrow follows `@trigger(...)`, whose arguments are not names.
func isLambdaArrow(toks []parser.Token, idx int) bool {
	if idx == 0 {
		return false
	}
	prev := toks[idx-1]
	if prev.Type == parser.TokenIdentifier {
		return !strings.Contains(prev.Literal, ".")
	}
	if prev.Type != parser.TokenParenClose {
		return false
	}
	for i := idx - 2; i >= 0; i-- {
		switch toks[i].Type {
		case parser.TokenParenOpen:
			return true
		case parser.TokenIdentifier, parser.TokenComma:
			continue
		default:
			return false
		}
	}
	return false
}

// isOperandStart reports whether a token is one after which a `-` is unary.
func isOperandStart(t parser.Token) bool {
	switch t.Type {
	case parser.TokenOperator, parser.TokenParenOpen, parser.TokenBracketOpen, parser.TokenComma,
		parser.TokenAmpAmp, parser.TokenPipePipe, parser.TokenQuestionQuestion, parser.TokenQuestion,
		parser.TokenColon, parser.TokenDefine, parser.TokenBang, parser.TokenKeywordReturn:
		return true
	}
	return false
}

// adjacent reports whether b starts exactly where a ends.
func adjacent(a, b parser.Token) bool {
	return a.EndLine == b.Line && a.EndCol == b.Column
}

// firstWordOfLine returns the literal of the first token on idx's line.
func firstWordOfLine(toks []parser.Token, idx int) string {
	i := idx
	for i > 0 && toks[i-1].Line == toks[idx].Line {
		i--
	}
	return toks[i].Literal
}

// ---- Retired spellings ----

// retiredForms are the retired spellings that are syntax rather than a
// function name, so the catalog's RetiredFunctions has no row for them. The
// parser's own refusal table (V1RetiredForms, on the parser branch) is the
// authority; these are the ones an author meets as a single token.
var retiredForms = map[string]struct{ spelling, replacement string }{
	"when": {"when(args.x) { ... }", "args.x == nil || <predicate>"},
	"null": {"null", "nil"},
	"has":  {"x has v", "v in x"},
}

// retiredAt returns the retired spelling the token at idx is, and what to
// write instead. A retired FUNCTION is its name used as a call -- `count` on
// its own line is the query's count clause, and `.count(` is the live list
// method -- and `contains` is retired only in its two-argument substring
// form: with one argument it is the live traversal.
func retiredAt(toks []parser.Token, idx, col int) (spelling, replacement string, ok bool) {
	t := toks[idx]
	calls := idx+1 < len(toks) && toks[idx+1].Type == parser.TokenParenOpen
	switch t.Type {
	case parser.TokenKeywordWhen:
		if calls {
			f := retiredForms["when"]
			return f.spelling, f.replacement, true
		}
	case parser.TokenKeywordHas:
		f := retiredForms["has"]
		return f.spelling, f.replacement, true
	case parser.TokenKeywordNot:
		if calls {
			return "not(a)", functions.RetiredFunctions()["not"], true
		}
		if idx+1 < len(toks) && toks[idx+1].Type == parser.TokenKeywordIn {
			return "x not in list", "!(x in list)", true
		}
	case parser.TokenIdentifier:
		seg, last, recv := segmentAt(t, col)
		if t.Literal == "null" {
			f := retiredForms["null"]
			return f.spelling, f.replacement, true
		}
		if !calls || !last {
			return "", "", false
		}
		if recv != "" {
			// A method: the collection `.contains(v)` is membership now.
			if r, ok := functions.RetiredMethods()["list."+seg]; ok {
				return "x." + seg + "(v)", r, true
			}
			return "", "", false
		}
		if seg == "contains" {
			if callArgCount(toks, idx+1) >= 2 {
				includes, _ := functions.Method(functions.TypeString, "includes")
				return "contains(s, sub)", methodSpelling(includes, "contains(s, sub)"), true
			}
			return "", "", false
		}
		if r, ok := functions.RetiredFunctions()[seg]; ok {
			return retiredSpelling(seg), r, true
		}
	}
	return "", "", false
}

// retiredSpelling returns how a retired function was written: the catalog
// entry that replaces it records the spelling ("len(x)"), otherwise its name
// over an ellipsis.
func retiredSpelling(name string) string {
	for _, f := range functions.Catalog() {
		for _, r := range f.Retired {
			if strings.HasPrefix(r, name+"(") {
				return r
			}
		}
	}
	return name + "(...)"
}

// methodSpelling renders the call a retired function spelling becomes as a
// method: its first argument turns into the receiver, `contains(s, sub)` into
// `s.includes(sub)`.
func methodSpelling(m functions.Function, retired string) string {
	open := strings.IndexByte(retired, '(')
	args := strings.Split(strings.TrimSuffix(retired[open+1:], ")"), ",")
	for i := range args {
		args[i] = strings.TrimSpace(args[i])
	}
	return args[0] + "." + m.Name + "(" + strings.Join(args[1:], ", ") + ")"
}

// retiredCard renders a retired spelling's hover card: the replacement, then
// the retirement and the rewrite that performs it.
func retiredCard(spelling, replacement string) string {
	return fmt.Sprintf("```memql\n%s\n```\n\n`%s` is retired in edition 2026. Write `%s` instead; `%s` rewrites it.",
		replacement, spelling, replacement, migrator)
}

// callArgCount counts the top-level arguments of the call whose `(` is at
// toks[open].
func callArgCount(toks []parser.Token, open int) int {
	depth, args, seen := 0, 0, false
	for i := open; i < len(toks); i++ {
		switch toks[i].Type {
		case parser.TokenParenOpen, parser.TokenBracketOpen, parser.TokenBraceOpen:
			depth++
			if depth == 1 {
				continue
			}
		case parser.TokenParenClose, parser.TokenBracketClose, parser.TokenBraceClose:
			depth--
			if depth == 0 {
				if seen {
					args++
				}
				return args
			}
		case parser.TokenComma:
			if depth == 1 {
				args++
				seen = false
				continue
			}
		}
		if depth >= 1 {
			seen = true
		}
	}
	if seen {
		args++
	}
	return args
}

// ---- Catalog functions and methods ----

// catalogEntryAt returns the catalog function or method the token at idx calls,
// with what the card needs to know about the call.
func (s *Service) catalogEntryAt(toks []parser.Token, idx, line, col int, ctx CursorContext) (functions.Function, callSite, bool) {
	t := toks[idx]
	if t.Type != parser.TokenIdentifier || idx+1 >= len(toks) || toks[idx+1].Type != parser.TokenParenOpen {
		return functions.Function{}, callSite{}, false
	}
	seg, last, recv := segmentAt(t, col)
	if !last {
		return functions.Function{}, callSite{}, false
	}
	free := callArgsRowFree(toks, idx+1, ctx.Params)
	if recv == "" {
		f, ok := functions.Lookup(seg)
		if !ok {
			return functions.Function{}, callSite{}, false
		}
		return f, callSite{rowField: rowFieldOf(toks, idx+1, ctx.Param), rowFree: free}, true
	}
	f, ok := s.resolveMethod(ctx, recv, seg, line)
	return f, callSite{receiver: recv, rowFree: free && !readsParam(recv, ctx.Params)}, ok
}

// segmentAt splits a fused dotted identifier (`row.tags.any`) at the cursor:
// the segment under it, whether that is the last segment, and the path before
// the last dot ("" for an undotted name).
func segmentAt(t parser.Token, col int) (seg string, last bool, recv string) {
	lit := t.Literal
	off := col - t.Column
	if off < 0 {
		off = 0
	}
	start := strings.LastIndexByte(lit[:min(off, len(lit))], '.') + 1
	end := len(lit)
	if i := strings.IndexByte(lit[start:], '.'); i >= 0 {
		end = start + i
	}
	seg = lit[start:end]
	last = end == len(lit)
	if i := strings.LastIndexByte(lit, '.'); i >= 0 && last {
		recv = lit[:i]
	}
	return seg, last, recv
}

// rowFieldOf returns the member of the row the call's first argument reads:
// `email` in `lower(row.email)`.
func rowFieldOf(toks []parser.Token, open int, param string) string {
	if param == "" || open+1 >= len(toks) || toks[open+1].Type != parser.TokenIdentifier {
		return ""
	}
	if rest, ok := strings.CutPrefix(toks[open+1].Literal, param+"."); ok && !strings.Contains(rest, ".") {
		return rest
	}
	return ""
}

// resolveMethod picks the catalog method a call names: the receiver's type
// decides where it is known (a member of the row, a declared arg), otherwise
// the one receiver that has a method of that name, and a list where two do.
func (s *Service) resolveMethod(ctx CursorContext, recv, name string, line int) (functions.Function, bool) {
	head, rest, _ := strings.Cut(recv, ".")
	typ := ""
	if p, ok := paramInScope(ctx.Params, head); ok && p.Callee == "" && rest != "" && !strings.Contains(rest, ".") {
		typ = s.memberType(ctx, rest)
	} else if head == "args" && rest != "" {
		typ = receiverForType(declaredArgType(ctx.Source, line, rest))
	}
	if typ != "" {
		if f, ok := functions.Method(typ, name); ok {
			return f, true
		}
	}
	var found []functions.Function
	for _, m := range functions.Catalog() {
		if m.Receiver != "" && m.Name == name {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return functions.Function{}, false
	case 1:
		return found[0], true
	}
	return functions.Method(functions.TypeList, name)
}

// catalogCard renders a catalog function's or method's hover card at a
// position. a is the manifest's admission of the entry there.
func catalogCard(f functions.Function, pos tiers.Position, a tiers.Admission, param string, site callSite) string {
	var b strings.Builder
	fmt.Fprintf(&b, "```memql\n%s\n```\n\n%s\n\n%s", f.Signature(), firstSentence(f.Doc), runsLine(f, pos, a))
	if line := legalityLine(f, pos, a, param, site); line != "" {
		b.WriteString("\n\n" + line)
	}
	if len(f.Retired) > 0 {
		b.WriteString("\n\n" + replacesLine(f.Retired))
	}
	return b.String()
}

// runsLine says where the entry runs, in plain words, at the cursor's
// position: pushed down, in process, or in process before the query.
func runsLine(f functions.Function, pos tiers.Position, a tiers.Admission) string {
	const (
		pushed = "Pushed down to SQL."
		inProc = "Runs in process."
		before = "Runs in process before the query, on values that do not read the row."
	)
	if pos != "" && tiers.TierOf(pos) == tiers.TierP {
		switch a {
		case tiers.Admitted:
			return pushed
		case tiers.PlanConstantOnly:
			return before
		}
	}
	if pos != "" && tiers.TierOf(pos) == tiers.TierM {
		return inProc
	}
	if f.Tier == functions.TierP {
		return pushed
	}
	return inProc
}

// legalityLine is present only when the position refuses or restricts the
// entry, and names the fix in one sentence with a code span.
func legalityLine(f functions.Function, pos tiers.Position, a tiers.Admission, param string, site callSite) string {
	if pos == "" {
		return ""
	}
	switch a {
	case tiers.PlanConstantOnly:
		if site.rowFree {
			return ""
		}
		what := "`" + f.Name + "`"
		if f.Receiver == functions.TypeString {
			what = "`" + f.Name + "` on a string"
		}
		return fmt.Sprintf("Not allowed on the row in %s: %s runs in process. %s", positionPhrase(pos), what, pushdownFix(f, param, site))
	case tiers.Refused:
		return fmt.Sprintf("Not allowed in %s, which %s.", positionPhrase(pos), literalRule(pos))
	}
	return ""
}

// pushdownFix is the nearest pushdown spelling for an in-process entry used on
// the row: compute the value from things that do not read the row and compare
// the row against it -- or, over a row array, the methods that push down.
func pushdownFix(f functions.Function, param string, site callSite) string {
	if param == "" {
		param = "row"
	}
	switch f.Receiver {
	case functions.TypeList:
		recv := site.receiver
		if recv == "" {
			recv = param + ".items"
		}
		return fmt.Sprintf("Over a row array, use `any`, `all` or `count`: `%s.any(x => x == args.value)`.", recv)
	case functions.TypeString:
		return fmt.Sprintf("Compute it from a value that does not read the row: `args.value.%s()`.", f.Name)
	}
	field := site.rowField
	if field == "" {
		field = "field"
	}
	args := make([]string, len(f.Params))
	for i, p := range f.Params {
		args[i] = exampleArg(p, field, i == 0)
	}
	op := "=="
	switch f.Returns {
	case functions.TypeDatetime:
		op = "<"
	case functions.TypeNumber:
		op = ">"
	}
	return fmt.Sprintf("Compare the row against a computed value: `%s.%s %s %s(%s)`.", param, field, op, f.Name, strings.Join(args, ", "))
}

// exampleArg is an argument the fix can show for a parameter: a value that
// does not read the row.
func exampleArg(p functions.Param, field string, first bool) string {
	switch p.Type {
	case functions.TypeDatetime:
		return "now"
	case functions.TypeDuration:
		return `"P1D"`
	case functions.TypeNumber:
		return "args.limit"
	case functions.TypeBool:
		return "true"
	case functions.TypeLambda:
		return "x => x"
	}
	if first {
		return "args." + field
	}
	return "args." + p.Name
}

// literalRule says what a literal position takes.
func literalRule(pos tiers.Position) string {
	switch pos {
	case tiers.PositionSort:
		return "takes a literal field name such as `\"row.createdAt\"`"
	case tiers.PositionRowAuthzArgument:
		return "takes a literal such as `owner=\"ownerUserId\"`"
	case tiers.PositionToolDefault:
		return "takes a literal value such as `@default(\"10\")`"
	}
	return "does not take it"
}

// replacesLine names the retired spellings an entry replaces.
func replacesLine(retired []string) string {
	quoted := make([]string, len(retired))
	for i, r := range retired {
		quoted[i] = "`" + r + "`"
	}
	switch len(quoted) {
	case 1:
		return "Replaces " + quoted[0] + "."
	case 2:
		return "Replaces " + quoted[0] + " and " + quoted[1] + "."
	}
	return "Replaces " + strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1] + "."
}

// firstSentence returns a doc's first sentence: the card carries one, and the
// rest of the doc is a hover away in the catalog.
func firstSentence(doc string) string {
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}
