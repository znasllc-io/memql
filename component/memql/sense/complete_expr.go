package sense

// complete_expr.go is completion at an expression position (memql#5365): it
// offers exactly what the tier manifest (component/language/tiers) admits at
// the cursor's position, drawn from the function catalog
// (component/language/functions) and the registry's predicates.
//
//   - A catalog function the position admits outright is offered with its
//     signature. One it admits only on plan constants -- an in-process
//     function in a pushdown position -- is offered too, because
//     `row.expiresAt < addDuration(now, "P1D")` is a legal filter, but its
//     detail says it computes before the query and cannot read the row. A
//     function the position refuses is not offered.
//   - Nothing the position refuses reaches the list: no construct call in a
//     filter or a condition, no retired spelling anywhere, no statement
//     keyword in the middle of an expression.
//   - After `<param>.` the parameter's members are offered -- the bound
//     concept's fields and the row intrinsics -- and after a member of a known
//     list or string type, the methods the position admits on it.

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// planConstantDetail is the completion detail of an item a pushdown position
// admits only where it does not read the row.
const planConstantDetail = "Computes before the query; cannot read the row"

// completeAtExpression answers completion at an expression position the
// manifest has rules for, and reports whether it did. A clause written as a
// lambda -- a query's filter and refine, a trigger @filter -- that has no
// header yet is offered the header and nothing else. It declines, leaving the
// existing completers in charge, at the literal positions (whose completion is
// the annotation's own keyword arguments) and at the other pre-v1 spellings of
// a pushdown position.
func (s *Service) completeAtExpression(ctx CursorContext, source string, line, col int) ([]CompletionItem, bool) {
	if ctx.Position == "" {
		return nil, false
	}
	if header, ok := lambdaHeaderOf(ctx); ok && !ctx.Lambda {
		// The clause has no lambda header yet. The parser refuses every other
		// spelling of it -- a filter over bare field names, `payload.` or a
		// bare `row.` intrinsic, a raw-text @filter -- so the header is the one
		// item, and only where it goes.
		return header.itemsAt(source, line, col, ctx.Prefix), true
	}
	// Inside an annotation's parentheses the classifier reports the annotation
	// before it looks for a dot, so `@filter(row => row.` arrives as annotation
	// arguments. At an expression position the dot is what matters.
	if ctx.Kind == ContextAnnotationArgs {
		if fa, ok := checkFieldAccessContext(textBeforeCursor(source, line, col)); ok {
			ctx.Kind, ctx.AccessorRoot, ctx.Prefix = ContextFieldAccess, fa.AccessorRoot, fa.Prefix
		}
	}
	if ctx.Kind == ContextFieldAccess {
		return s.completeExpressionMember(ctx, source, line)
	}
	switch ctx.Kind {
	case ContextFuncBody, ContextFuncCallArgs, ContextTopLevel, ContextAnnotationArgs:
	default:
		return nil, false
	}
	switch ctx.Position {
	case tiers.PositionSort, tiers.PositionRowAuthzArgument, tiers.PositionToolDefault:
		return nil, false
	}
	if tiers.TierOf(ctx.Position) == tiers.TierP && !ctx.Lambda {
		return nil, false
	}
	return s.completeExpression(ctx, source, line, col), true
}

// lambdaClauseHeader describes a clause whose value is a lambda: the text that
// opens it, up to where its header goes, and the header item it is offered
// while it has none.
type lambdaClauseHeader struct {
	opener      *regexp.Regexp
	detail, doc string
}

var (
	filterLambdaHeader = lambdaClauseHeader{
		opener: regexp.MustCompile(`^\s*filter\s*$`),
		detail: "filter lambda header",
		doc:    "Open the filter as a lambda over the row: `filter row => row.status == args.status`.",
	}
	refineLambdaHeader = lambdaClauseHeader{
		opener: regexp.MustCompile(`^\s*refine\s*$`),
		detail: "refine lambda header",
		doc:    "Open the clause as a lambda over each row of the page: `refine row => row.title.includes(args.q)`. It runs in process after `paginate` reads the page, so a page can come back with fewer rows.",
	}
	triggerFilterLambdaHeader = lambdaClauseHeader{
		opener: regexp.MustCompile(`@filter\s*\(\s*$`),
		detail: "trigger filter lambda header",
		doc:    "Open the trigger filter as a lambda over the row whose event fired it: `@filter(row => row.status == \"archived\")`.",
	}
)

// lambdaHeaderOf reports the header of the lambda clause at the cursor: a
// query's braceless filter, its refine clause, or a trigger @filter. The query
// a tool's @handler runs is a query filter too, but a string rather than a
// clause, and has no header to offer.
func lambdaHeaderOf(ctx CursorContext) (lambdaClauseHeader, bool) {
	switch {
	case ctx.Position == tiers.PositionQueryFilter && ctx.Enclosing.Keyword == "query" && !containsString(ctx.Enclosing.Blocks, "filter"):
		return filterLambdaHeader, true
	case ctx.Position == tiers.PositionQueryRefine:
		return refineLambdaHeader, true
	case ctx.Position == tiers.PositionTriggerFilter:
		return triggerFilterLambdaHeader, true
	}
	return lambdaClauseHeader{}, false
}

// itemsAt offers the header where it goes: straight after the clause's opener,
// with at most the word being typed in between. Anywhere else in a clause with
// no header -- `filter status == |` -- nothing is offered.
func (h lambdaClauseHeader) itemsAt(source string, line, col int, prefix string) []CompletionItem {
	const label = "row => ..."
	before := textBeforeCursor(source, line, col)
	cur := before[strings.LastIndexByte(before, '\n')+1:]
	if !h.opener.MatchString(strings.TrimSuffix(cur, prefix)) || !strings.HasPrefix(label, prefix) {
		return nil
	}
	return []CompletionItem{{
		Label: label, Kind: "snippet", Detail: h.detail, Documentation: h.doc,
		InsertText: "row => row.$0", IsSnippet: true, SortPriority: 1,
	}}
}

// completeExpression is the bare-name completion set of an expression
// position.
func (s *Service) completeExpression(ctx CursorContext, source string, line, col int) []CompletionItem {
	pos := ctx.Position
	var items []CompletionItem
	add := func(it CompletionItem) {
		if strings.HasPrefix(it.Label, ctx.Prefix) {
			items = append(items, it)
		}
	}
	// An expression in a body written in statements (epic memql#5370) reads
	// the roots the load gate admits there, and a construct call in it is a
	// statement's whole value, never part of an expression.
	statements := (pos == tiers.PositionLogicBody || pos == tiers.PositionStepArgument || pos == tiers.PositionAutomationCondition) &&
		inStatementBody(strings.Split(source, "\n"), line, cursorLine(source, line, col), ctx.Enclosing)

	// 1. The lambda parameters in scope, innermost last.
	seen := map[string]bool{}
	for i := len(ctx.Params) - 1; i >= 0; i-- {
		p := ctx.Params[i]
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		add(CompletionItem{
			Label: p.Name, Kind: "variable", Detail: "lambda parameter",
			Documentation: paramDocumentation(ctx, p), InsertText: p.Name, SortPriority: 1,
		})
	}

	// 2. The names statements above the cursor bound in this body
	// (`rows := query activeUsers()`): a bare name resolves to one before it
	// resolves to a predicate or a function.
	for _, l := range localsBefore(source, line) {
		if seen[l.name] {
			continue
		}
		seen[l.name] = true
		add(CompletionItem{
			Label: l.name, Kind: "variable", Detail: "local",
			Documentation: "Bound above: `" + l.name + " := " + l.rhs + "`.", InsertText: l.name, SortPriority: 1,
		})
	}

	// 3. The reserved roots this position evaluates with -- in a statement body
	// the load gate's (compiler.IsBodyRoot), which retire the step graph's
	// steps, item, index and input.
	for _, r := range positionRoots[pos] {
		if seen[r] || (statements && !compiler.IsBodyRoot(ctx.Enclosing.Keyword, r)) {
			continue
		}
		def := rootDefAt(pos, r)
		add(CompletionItem{
			Label: r, Kind: "variable", Detail: def.detail,
			Documentation: def.doc, InsertText: def.insert, SortPriority: 2,
		})
	}

	// 4. Catalog functions, as the manifest admits them here. (Predicates,
	// below, sort above them: in a predicate position they are what is
	// usually called.)
	for _, f := range functions.Catalog() {
		if f.Receiver != "" {
			continue
		}
		a := tiers.FunctionAdmission(pos, f.Key())
		if a == tiers.Refused {
			continue
		}
		// A function usable here only on plan constants sorts last: it is the
		// least likely thing a predicate over the row calls.
		priority := 4
		if a == tiers.PlanConstantOnly {
			priority = 7
		}
		add(CompletionItem{
			Label: f.Name, Kind: "function", Detail: catalogDetail(f, a, true),
			Documentation: catalogCard(f, pos, a, ctx.Param, callSite{}),
			InsertText:    f.Name + "(", SortPriority: priority,
		})
	}

	// 5. Predicates, applied to the receiver they read.
	if tiers.PredicateAdmission(pos) != tiers.Refused {
		for _, it := range s.predicateItems(ctx) {
			add(it)
		}
	}

	// 6. The literals.
	if tiers.Allows(pos, ast.KindNil) {
		add(CompletionItem{Label: "nil", Kind: "keyword", Detail: "the absent value", InsertText: "nil", SortPriority: 6})
	}
	if tiers.Allows(pos, ast.KindLiteral) {
		for _, lit := range []string{"true", "false"} {
			add(CompletionItem{Label: lit, Kind: "keyword", Detail: "boolean literal", InsertText: lit, SortPriority: 6})
		}
	}

	// 7. Construct calls, where the position admits them (a logic body, a step
	// argument): the kind-prefixed verbs. In a statement body a call is a
	// statement's whole value -- `x := <call>`, `return <call>` -- and refused
	// inside an expression (body_call_in_expression).
	switch {
	case statements:
		if statementValueStart.MatchString(strings.TrimSuffix(cursorLine(source, line, col), ctx.Prefix)) {
			items = append(items, s.statementCallItems(ctx.Prefix, ctx.Enclosing)...)
		}
	case tiers.Allows(pos, ast.KindConstructCall):
		for _, kw := range invocationKeywordsForConstruct(ctx.Enclosing) {
			add(CompletionItem{Label: kw, Kind: "keyword", Detail: "construct call", InsertText: kw + " ", SortPriority: 5})
		}
	}

	// 8. Statement keywords, only where a statement can start: the beginning of
	// a line in a logic body. Mid-expression, `if` or `return` is not an
	// expression and offering it would teach one. A statement body's starts are
	// the statement completer's (complete_statements.go), so a line reaching
	// here there continues an expression.
	if pos == tiers.PositionLogicBody && !statements && atStatementStart(source, line, col, ctx.Prefix) {
		for _, kw := range statementKeywords {
			add(CompletionItem{Label: kw, Kind: "keyword", Detail: "keyword", Documentation: KeywordDocs[kw], InsertText: kw, SortPriority: 10})
		}
	}

	// 9. A legacy automation's declared args resolve bare (G2, memql#2364). A
	// body written in statements reads them `args.x` (epic memql#5370) and
	// refuses the bare name, so it is not offered there.
	if ctx.Enclosing.Keyword == "automation" && !inStatementBody(strings.Split(source, "\n"), line, cursorLine(source, line, col), ctx.Enclosing) {
		items = append(items, automationArgsFieldCompletions(source, line, ctx.Prefix)...)
	}
	return items
}

// statementKeywords are the words that open a statement in a logic body, or
// trail one (edition 2026, epic memql#5370). `nil` is a literal (offered
// above), `publish` is an automation's only (D14), and `range`, `when`,
// `continue` and `break` are not statements.
var statementKeywords = []string{"if", "else", "for", "switch", "case", "default", "parallel", "branch", "return", "retry", "wait", "on"}

// atStatementStart reports whether the cursor line holds nothing before the
// prefix being typed.
func atStatementStart(source string, line, col int, prefix string) bool {
	before := textBeforeCursor(source, line, col)
	cur := before[strings.LastIndexByte(before, '\n')+1:]
	return strings.TrimSpace(strings.TrimSuffix(cur, prefix)) == ""
}

// completeExpressionMember is member completion at an expression position:
// after the position's own parameter, a nested lambda's parameter, a member of
// either, or a declared arg of a list or string type.
func (s *Service) completeExpressionMember(ctx CursorContext, source string, line int) ([]CompletionItem, bool) {
	root := ctx.AccessorRoot
	head, rest, _ := strings.Cut(root, ".")
	if p, ok := paramInScope(ctx.Params, head); ok {
		switch {
		case p.Callee == "" && rest == "":
			// The position's own parameter: the actor envelope for a spec over
			// an @actor shape, otherwise the row it predicates over.
			if head == "actor" {
				return actorMemberCompletions(ctx.Prefix), true
			}
			return s.rowMemberItems(ctx), true
		case p.Callee != "" && rest == "":
			// A traversal's match lambda ranges over rows of a concept the
			// traversal reaches; which one is not known here, so its intrinsics
			// are what can be offered. Any other nested parameter is an
			// element of a value whose shape is not known.
			if isTraversal(p.Callee) {
				return rowIntrinsicItems(ctx.Prefix), true
			}
			return nil, true
		case p.Callee == "" && rest != "" && !strings.Contains(rest, "."):
			// `row.tags.` -- a method on a member of the row.
			return s.methodItems(ctx, s.memberType(ctx, rest), true), true
		}
		return nil, true
	}
	if head == "args" {
		fields, types := argsAt(ctx, source, line)
		switch {
		case rest == "" && ctx.Position == tiers.PositionTriggerFilter:
			return argsFieldItems(fields, types, ctx.Prefix,
				"Declared in the automation's args { } block, bound from the triggering event's payload."), true
		case rest != "" && !strings.Contains(rest, "."):
			if typ := types[rest]; typ != "" {
				return s.methodItems(ctx, receiverForType(typ), false), true
			}
		}
	}
	if rest == "" {
		// A local bound to a query's result is a list of rows.
		for _, l := range localsBefore(source, line) {
			if l.name == head && startsWithWord(l.rhs, "query") {
				return s.methodItems(ctx, functions.TypeList, false), true
			}
		}
	}
	if head == "payload" {
		// `payload.` was the pre-v1 spelling of a payload field. Edition 2026
		// reads a field through the position's parameter (`row.title`), and
		// refuses a bare `payload` as a name nothing binds, so it offers nothing.
		return nil, true
	}
	return nil, false
}

// local is a name a statement bound: `rows := query activeUsers()`.
type local struct {
	name, rhs string
}

// localAssignment reads one `name := <rhs>` statement line.
var localAssignment = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:=\s*(.*?)\s*$`)

// localsBefore returns the names bound by `name := ...` statements above the
// cursor line in the construct around it: a later statement's name is not in
// scope yet (D12 refuses a forward reference), so only the lines above count.
// The scan stops at the construct's header -- a line that opens a construct.
func localsBefore(source string, line int) []local {
	lines := strings.Split(source, "\n")
	var out []local
	for i := line - 1; i >= 1 && i <= len(lines); i-- {
		text := strings.TrimSpace(lines[i-1])
		if first := firstWord(text); first != "" && dslSpec.ConstructByKeyword(first) != nil && strings.HasSuffix(text, "{") {
			break
		}
		if m := localAssignment.FindStringSubmatch(lines[i-1]); m != nil {
			out = append(out, local{name: m[1], rhs: m[2]})
		}
	}
	return out
}

// paramInScope finds a lambda parameter by name, innermost first.
func paramInScope(params []LambdaParam, name string) (LambdaParam, bool) {
	for i := len(params) - 1; i >= 0; i-- {
		if params[i].Name == name {
			return params[i], true
		}
	}
	return LambdaParam{}, false
}

// rowMemberItems is what `row.` offers at a position whose parameter is a row:
// the bound concept's fields, each with its type, then the row intrinsics.
func (s *Service) rowMemberItems(ctx CursorContext) []CompletionItem {
	var items []CompletionItem
	if c, ok := s.boundConcept(ctx); ok {
		for _, f := range c.Fields {
			if strings.HasPrefix(f.Name, ctx.Prefix) {
				items = append(items, CompletionItem{
					Label: f.Name, Kind: "field", Detail: f.Type,
					Documentation: f.Description, InsertText: f.Name, SortPriority: 1,
				})
			}
		}
	}
	return append(items, rowIntrinsicItems(ctx.Prefix)...)
}

// rowIntrinsicItems offers the intrinsics every stored row carries.
func rowIntrinsicItems(prefix string) []CompletionItem {
	var items []CompletionItem
	for _, m := range rowIntrinsics {
		if strings.HasPrefix(m.Name, prefix) {
			items = append(items, CompletionItem{
				Label: m.Name, Kind: "field", Detail: "row intrinsic",
				Documentation: m.Doc, InsertText: m.Name, SortPriority: 2,
			})
		}
	}
	return items
}

// boundConcept resolves the position's bound name to a registry concept: the
// short name the signature binds, disambiguated by the file's own domain.
func (s *Service) boundConcept(ctx CursorContext) (*ConceptInfo, bool) {
	if s.registries == nil || ctx.Bound == "" {
		return nil, false
	}
	canonical, ok := s.resolveBareConcept(ctx.Bound, ctx.FilePath)
	if !ok {
		return nil, false
	}
	return s.registries.ConceptGet(canonical)
}

// memberType returns the catalog receiver a member of the position's row has
// -- "list" or "string" -- from the bound concept's declared field type.
func (s *Service) memberType(ctx CursorContext, field string) string {
	c, ok := s.boundConcept(ctx)
	if !ok {
		return ""
	}
	for _, f := range c.Fields {
		if f.Name == field {
			return receiverForType(f.Type)
		}
	}
	return ""
}

// receiverForType maps a declared type -- a concept field's JSON-schema type
// ("array", "string") or an args field's DSL type ("[]string", "string!") --
// to the catalog receiver whose methods apply to it.
func receiverForType(typ string) string {
	t := strings.TrimSuffix(strings.TrimSpace(typ), "!")
	switch {
	case t == "array" || t == "list" || strings.HasPrefix(t, "[]"):
		return functions.TypeList
	case t == "string":
		return functions.TypeString
	}
	return ""
}

// methodItems offers the catalog methods of a receiver the position admits.
// readsRow is true when the receiver is a member of the row: an in-process
// method is then refused however it is written, so only the methods that push
// down are offered. On a value that does not read the row, every admitted
// method is legal and is offered with its signature.
func (s *Service) methodItems(ctx CursorContext, receiver string, readsRow bool) []CompletionItem {
	if receiver == "" {
		return nil
	}
	var items []CompletionItem
	for _, m := range functions.Catalog() {
		if m.Receiver != receiver && m.Receiver != functions.TypeAny {
			continue
		}
		a := tiers.FunctionAdmission(ctx.Position, m.Key())
		if a == tiers.Refused || (readsRow && a == tiers.PlanConstantOnly) {
			continue
		}
		if !strings.HasPrefix(m.Name, ctx.Prefix) {
			continue
		}
		insert := m.Name + "("
		if len(m.Params) == 0 {
			insert = m.Name + "()"
		}
		items = append(items, CompletionItem{
			Label: m.Name, Kind: "method", Detail: catalogDetail(m, a, readsRow),
			Documentation: catalogCard(m, ctx.Position, a, ctx.Param, callSite{}),
			InsertText:    insert, SortPriority: 1,
		})
	}
	return items
}

// catalogDetail is a catalog entry's completion detail: its signature, or --
// where the position admits it only on plan constants and nothing says the
// value is one -- the reason it cannot read the row.
func catalogDetail(f functions.Function, a tiers.Admission, mayReadRow bool) string {
	if a == tiers.PlanConstantOnly && mayReadRow {
		return planConstantDetail
	}
	return f.Signature()
}

// predicateItems offers the registry's specs and traits, each applied to the
// receiver it predicates over: a trait or a row spec to the position's row
// parameter, a context spec to `actor`. A row spec bound to a DIFFERENT
// concept than the position's is not offered: its body reads that concept's
// fields, not this row's.
func (s *Service) predicateItems(ctx CursorContext) []CompletionItem {
	if s.registries == nil {
		return nil
	}
	var items []CompletionItem
	for _, name := range s.registries.SpecNames() {
		info, _ := s.registries.SpecGet(name)
		arg, detail := ctx.Param, "trait"
		switch {
		case info != nil && info.Kind == "context":
			arg, detail = "actor", "predicate over the actor"
		case info != nil && !info.Trait && info.Bound != "":
			if ctx.Bound != "" && info.Bound != ctx.Bound && s.isConceptName(info.Bound) {
				continue
			}
			detail = "spec over " + info.Bound
		}
		doc := ""
		if info != nil {
			doc = info.Description
		}
		it := CompletionItem{
			Label: name + "(" + arg + ")", Kind: "spec", Detail: detail,
			Documentation: doc, InsertText: name + "(" + arg + ")", SortPriority: 3,
		}
		if arg == "" {
			// An in-process position has no row parameter of its own: the
			// argument is the author's to name.
			it.Label = name + "(row)"
			it.InsertText = name + "(${1:row})"
			it.IsSnippet = true
		}
		items = append(items, it)
	}
	return items
}

// isConceptName reports whether a bare name is a registry concept's short name
// -- as opposed to a shape's, which a spec may also bind.
func (s *Service) isConceptName(name string) bool {
	for _, canonical := range s.registries.ConceptNames() {
		if _, short := splitConceptID(canonical); short == name {
			return true
		}
	}
	return false
}

// paramDocumentation explains a lambda parameter in completion.
func paramDocumentation(ctx CursorContext, p LambdaParam) string {
	switch {
	case p.Callee != "" && isTraversal(p.Callee):
		return "A row `" + p.Callee + "` reaches."
	case p.Callee != "":
		return "The element `" + p.Callee + "` passes to its lambda."
	case p.Name == "actor":
		return "The actor envelope this predicate reads."
	case ctx.Position == tiers.PositionTriggerFilter:
		return "The row whose change fired the trigger."
	case ctx.Position == tiers.PositionQueryRefine:
		return "A row of the page the query read."
	}
	return "The row this predicate is asked about."
}

// rootDef describes one reserved root in completion.
type rootDef struct {
	detail, doc, insert string
}

var rootDefs = map[string]rootDef{
	"args":   {"caller arguments", "The arguments the caller passed, as declared in `args { }`.", "args."},
	"actor":  {"the acting identity", "The resolved auth context: userId, role, identityId, isClusterOwner.", "actor."},
	"now":    {"the evaluation clock", "The RFC 3339 timestamp captured when evaluation began.", "now"},
	"config": {"allow-listed configuration", "Configuration values the engine exposes to the language.", "config."},
	"event":  {"the triggering event", "The event envelope: topic, kind, payload, actor, timestamp.", "event."},
	"steps":  {"earlier step results", "The results of the steps this run has already taken.", "steps."},
	"item":   {"the loop element", "The element of the enclosing forEach.", "item"},
	"index":  {"the loop index", "The position of the element in the enclosing forEach.", "index"},
	"input":  {"the automation input", "The rows the automation's input query loaded.", "input"},
}

// rootDefAt is a root's completion entry at a position. A trigger filter has
// no caller: its `args` are the automation's declared args, bound from the
// triggering event's payload before the filter runs (D15).
func rootDefAt(pos tiers.Position, root string) rootDef {
	def := rootDefs[root]
	if root == "args" && pos == tiers.PositionTriggerFilter {
		def.detail = "payload arguments"
		def.doc = "The automation's declared args, bound from the triggering event's payload before the filter runs."
	}
	return def
}

// positionRoots are the reserved roots an expression at each position
// evaluates with (the in-process scope the evaluator binds, and the plan
// constants a pushdown position folds). A spec body reads its parameter and
// the clock; a query's refine clause reads what its filter reads; the
// automation positions add the run's roots.
var positionRoots = map[tiers.Position][]string{
	tiers.PositionQueryFilter:         {"args", "actor", "now", "config"},
	tiers.PositionQueryRefine:         {"args", "actor", "now", "config"},
	tiers.PositionSpecBody:            {"now"},
	tiers.PositionTriggerFilter:       {"args", "now", "config"},
	tiers.PositionAutomationCondition: {"args", "actor", "now", "config", "event"},
	tiers.PositionLogicBody:           {"args", "actor", "now", "config"},
	tiers.PositionMutationValue:       {"args", "actor", "now", "config"},
	tiers.PositionStepArgument:        {"args", "actor", "now", "config", "event"},
	tiers.PositionPromptInput:         {"args", "now", "config"},
}

// argsFieldTypePattern reads one args-block field line: its name and type.
var argsFieldTypePattern = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s+(\S+)`)

// argsAt returns the declared args an expression at the cursor reads as
// `args`, in order, with each field's type: the enclosing construct's, or --
// for a trigger filter, an annotation written ABOVE its automation -- the
// args of the automation the annotation decorates.
func argsAt(ctx CursorContext, source string, line int) ([]string, map[string]string) {
	if ctx.Position == tiers.PositionTriggerFilter {
		return decoratedConstructArgs(source, line)
	}
	return constructArgs(source, line, enclosingConstructHeader)
}

// enclosingHeaderRE is enclosingConstructHeader compiled once.
var enclosingHeaderRE = regexp.MustCompile(enclosingConstructHeader)

// decoratedConstructArgs returns the args of the construct an annotation on
// line decorates: the first construct header below it. A line that opens any
// other construct first means the annotation decorates something without an
// args block.
func decoratedConstructArgs(source string, line int) ([]string, map[string]string) {
	lines := strings.Split(source, "\n")
	for i := line; i < len(lines); i++ {
		if enclosingHeaderRE.MatchString(lines[i]) {
			return constructArgs(source, i+1, enclosingConstructHeader)
		}
		if first := firstWord(strings.TrimSpace(lines[i])); first != "" && dslSpec.ConstructByKeyword(first) != nil {
			return nil, nil
		}
	}
	return nil, nil
}
