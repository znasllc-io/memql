package sense

// exprpos.go answers "which expression position is the cursor in" -- the
// tiers.Position the tier manifest keys every admission rule by -- together
// with the lambda parameters in scope and the concept the position's
// parameter reads (memql#5365).
//
// It reads source TEXT, not the v1 parser, and that is deliberate: an editor's
// buffer is mid-edit and unparseable most of the time, so a detector that
// needs a parse would answer nothing exactly while an author is typing. The
// shapes it reads are the record's surface forms:
//
//	filter row => <predicate>             a query filter, and its continuation lines
//	refine row => <expression>            a query's refine clause, over the page paginate read
//	spec <bound> <name> = row => ...      a spec body (actor => over an @actor shape)
//	trait <name> = row => ...             a trait body
//	@filter(row => ...)                   a trigger filter over the triggering row
//	@rowAuthz(...), @default(...)         the literal positions
//	@handler(query="...")                 the query a tool's handler runs
//	if / forEach / switch / check:        automation conditions
//	<kind> <name>(...) in an automation   step arguments
//	a statement in a logic body           logicBody
//	key: <value> in insert/update/stamp   mutation values
//
// The pre-v1 spellings of the pushdown positions (a braceless filter with no
// lambda header, a `filter { }` block, a `spec ... { return ... }` body) are
// detected too, with lambda false: the tree is migrated in this epic, and until
// it is those clauses are still queries an author edits.

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/dslclause"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// LambdaParam is one lambda parameter in scope at the cursor.
type LambdaParam struct {
	// Name is the parameter as the author spelled it.
	Name string
	// Callee is the call the lambda is an argument of -- "childOf",
	// "row.tags.any" -- or "" for the position's own lambda (the `row` of
	// `filter row => ...`).
	Callee string
}

// exprPos is what the detector learns about the cursor.
type exprPos struct {
	position tiers.Position
	// param is the position's own lambda parameter ("" when it has none).
	param string
	// nested are the lambda parameters opened INSIDE the expression and still
	// in scope at the cursor, outermost first.
	nested []LambdaParam
	// bound is the concept (or shape) the position's parameter reads.
	bound string
	// lambda is true when the position opens with a lambda header -- the v1
	// spelling of a pushdown position.
	lambda bool
}

var (
	// lambdaHeader matches the start of a v1 lambda, `row =>` or `(row) =>`,
	// anchored at the start of the text it is applied to. Group 1 or 2 is the
	// parameter.
	lambdaHeader = regexp.MustCompile(`^\s*(?:([A-Za-z_][A-Za-z0-9_]*)|\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\))\s*=>`)
	// predicateHeader matches a v1 spec or trait declaration up to its arrow:
	// `spec todo isOverdue = row =>`, `trait isOpen = row =>`. For a spec,
	// group 2 is the bound name and group 3 the spec's name; for a trait,
	// group 2 is the name. Group 4 or 5 is the parameter.
	predicateHeader = regexp.MustCompile(`^\s*(spec|trait)\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s+([A-Za-z_][A-Za-z0-9_]*))?\s*=\s*(?:([A-Za-z_][A-Za-z0-9_]*)|\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\))\s*=>`)
	// triggerConceptKwarg / triggerEventConcept read the concept an automation
	// fires on: `@trigger(concept="v1:todos:todo")`, or the graph event topic
	// `@trigger(event="graph.node.updated.v1:todos:todo")`.
	triggerConceptKwarg = regexp.MustCompile(`@trigger\([^)]*\bconcept\s*=\s*"([^"]+)"`)
	triggerEventConcept = regexp.MustCompile(`@trigger\([^)]*\bevent\s*=\s*"graph\.node\.[A-Za-z]+\.([^"]+)"`)
	// mutationValueKey matches a write-block line up to its value: `title: `.
	mutationValueKey = regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z0-9_.]*\s*:`)
	// handlerQueryValue matches the text of a @handler argument list up to the
	// opening quote of its query= value.
	handlerQueryValue = regexp.MustCompile(`\bquery\s*=\s*$`)
)

// detectExprPosition classifies the cursor.
func detectExprPosition(source string, line, col int) exprPos {
	lines := strings.Split(source, "\n")
	if line < 1 || line > len(lines) {
		return exprPos{}
	}
	before := textBeforeCursor(source, line, col)
	scan := scanText(before)
	if scan.inComment {
		return exprPos{}
	}
	// The lexer returns NOTHING for text ending in an unterminated string, so
	// the enclosing construct is read from the text before the string opens.
	lexable := before
	if scan.inString {
		lexable = before[:scan.stringStart]
	}
	enc, inConstruct := resolveEnclosingConstruct(lexTokens(lexable))
	cur := before[strings.LastIndexByte(before, '\n')+1:]

	if p, ok := annotationPosition(lines, before, scan, enc); ok {
		return p
	}
	if inConstruct {
		return bodyPosition(lines, line, before, cur, scan, enc)
	}
	return predicateDeclPosition(lines, line, before, cur)
}

// lexTokens lexes text and drops the EOF token.
func lexTokens(text string) []parser.Token {
	toks, _ := parser.NewLexer(text).Tokenize()
	if n := len(toks); n > 0 && toks[n-1].Type == parser.TokenEOF {
		toks = toks[:n-1]
	}
	return toks
}

// annotationPosition handles a cursor inside an annotation's argument list. It
// answers ok for EVERY annotation the cursor is inside: one that is not an
// expression position (@trigger, @description) is still not the body below it.
func annotationPosition(lines []string, before string, scan textScan, enc EnclosingConstruct) (exprPos, bool) {
	ann, ok := scan.outermostAnnotation()
	if !ok {
		return exprPos{}, false
	}
	args := before[ann.offset+1:]
	switch ann.annotation {
	case "filter":
		p := exprPos{position: tiers.PositionTriggerFilter}
		if m := lambdaHeader.FindStringSubmatchIndex(args); m != nil {
			p.param = firstGroup(args, m, 2, 4)
			p.lambda = true
			p.nested = nestedLambdaParams(args[m[1]:])
		}
		p.bound = triggerConcept(lines, lineOfOffset(before, ann.offset))
		return p, true
	case "rowAuthz":
		return exprPos{position: tiers.PositionRowAuthzArgument}, true
	case "default":
		switch enc.Keyword {
		case "tool":
			return exprPos{position: tiers.PositionToolDefault}, true
		case "prompt":
			return exprPos{position: tiers.PositionPromptInput}, true
		}
	case "handler":
		// The query a tool handler runs is a query filter; the handler's other
		// arguments (type=, name=) are not expressions.
		if scan.inString && scan.stringStart > ann.offset && handlerQueryValue.MatchString(before[ann.offset+1:scan.stringStart]) {
			return exprPos{position: tiers.PositionQueryFilter}, true
		}
	}
	return exprPos{}, true
}

// bodyPosition handles a cursor inside a construct's braces.
func bodyPosition(lines []string, line int, before, cur string, scan textScan, enc EnclosingConstruct) exprPos {
	last := ""
	if n := len(enc.Blocks); n > 0 {
		last = enc.Blocks[n-1]
	}
	if last == "args" {
		return exprPos{}
	}
	code := strings.TrimSpace(blankLineCommentsAndStrings(cur))
	switch enc.Keyword {
	case "query":
		if last == "filter" {
			// The pre-v1 `filter { }` block.
			return exprPos{position: tiers.PositionQueryFilter, bound: enc.Concept}
		}
		return queryClausePosition(lines, line, before, cur, enc)

	case "spec", "trait":
		// The pre-v1 `spec <bound> <name> { return ... }` body.
		return exprPos{position: tiers.PositionSpecBody, bound: enc.Concept}

	case "logic":
		if containsString(enc.Blocks, "body") {
			return exprPos{position: tiers.PositionLogicBody, nested: nestedLambdaParams(strings.TrimLeft(cur, " \t"))}
		}

	case "automation", "action":
		switch {
		case conditionLine(code):
			return exprPos{position: tiers.PositionAutomationCondition, nested: nestedLambdaParams(code)}
		case last == "precondition" && strings.HasPrefix(code, "check:"):
			return exprPos{position: tiers.PositionAutomationCondition}
		case scan.insideConstructCall(), strings.Contains(code, ":="):
			return exprPos{position: tiers.PositionStepArgument, nested: nestedLambdaParams(strings.TrimLeft(cur, " \t"))}
		}

	case "mutate":
		switch last {
		case "insert", "update", "stamp":
			if mutationValueKey.MatchString(cur) {
				return exprPos{position: tiers.PositionMutationValue}
			}
		}
	}
	return exprPos{}
}

// conditionLine reports whether a line (trimmed, strings blanked) is the head
// of an automation condition the cursor has not yet left: `if`, `} else if`,
// `forEach ... [where ...]`, `for`, `switch` -- before the block's own `{`.
func conditionLine(code string) bool {
	if strings.Contains(code, "{") && !strings.HasPrefix(code, "}") {
		return false
	}
	t := strings.TrimSpace(strings.TrimPrefix(code, "}"))
	if startsWithWord(t, "else") {
		t = strings.TrimSpace(t[len("else"):])
	}
	if strings.Contains(t, "{") {
		return false
	}
	return startsWithWord(t, "if") || startsWithWord(t, "forEach") || startsWithWord(t, "for") || startsWithWord(t, "switch")
}

// queryClausePosition finds the clause of a struct-form query body the cursor
// line belongs to: the nearest clause opener above it, provided every line in
// between continues that clause's expression (see continuesExpression).
func queryClausePosition(lines []string, line int, before, cur string, enc EnclosingConstruct) exprPos {
	for i := line; i >= 1; i-- {
		text := lineText(lines, i, line, cur)
		code := strings.TrimSpace(blankLineCommentsAndStrings(text))
		if dslclause.StartsAnyOf(code, dslclause.StructQueryDirectives) || startsWithWord(code, "refine") {
			if !reachesCursor(lines, i, line, cur) {
				return exprPos{}
			}
			switch {
			case dslclause.StartsWith(code, "filter"):
				return lambdaClause(tiers.PositionQueryFilter, "filter", before, text, i, enc)
			case startsWithWord(code, "refine"):
				return lambdaClause(tiers.PositionQueryRefine, "refine", before, text, i, enc)
			case dslclause.StartsWith(code, "sort"):
				return exprPos{position: tiers.PositionSort}
			}
			// shape / paginate / count / asOf are not tier positions.
			return exprPos{}
		}
		if i != line && (code == "" || strings.HasSuffix(code, "{") || strings.HasPrefix(code, "}") || strings.HasPrefix(code, "@")) {
			return exprPos{}
		}
	}
	return exprPos{}
}

// lambdaClause reads a query clause whose value is a lambda over the query's
// rows -- `filter` (pushed down) or `refine` (in process, over the page) --
// opened by keyword on line i: its lambda header, if it has one yet, and the
// lambda parameters opened after the header. The parameter reads the query's
// bound concept either way.
func lambdaClause(pos tiers.Position, keyword, before, text string, i int, enc EnclosingConstruct) exprPos {
	p := exprPos{position: pos, bound: enc.Concept}
	indent := len(text) - len(strings.TrimLeft(text, " \t"))
	rest := text[indent+len(keyword):]
	m := lambdaHeader.FindStringSubmatchIndex(rest)
	if m == nil {
		return p
	}
	p.param = firstGroup(rest, m, 2, 4)
	p.lambda = true
	if start := lineStartOffset(before, i) + indent + len(keyword) + m[1]; start <= len(before) {
		p.nested = nestedLambdaParams(before[start:])
	}
	return p
}

// predicateDeclPosition handles a v1 spec or trait, which has no braces and so
// no enclosing construct: its header `spec <bound> <name> = row =>` sits on the
// cursor line, or on the line a run of continuation lines hangs from.
func predicateDeclPosition(lines []string, line int, before, cur string) exprPos {
	for i := line; i >= 1; i-- {
		text := lineText(lines, i, line, cur)
		if m := predicateHeader.FindStringSubmatchIndex(text); m != nil {
			if !reachesCursor(lines, i, line, cur) {
				return exprPos{}
			}
			p := exprPos{position: tiers.PositionSpecBody, lambda: true, param: firstGroup(text, m, 8, 10)}
			if text[m[2]:m[3]] == "spec" {
				p.bound = text[m[4]:m[5]]
			}
			if start := lineStartOffset(before, i) + m[1]; start <= len(before) {
				p.nested = nestedLambdaParams(before[start:])
			}
			return p
		}
		code := strings.TrimSpace(blankLineCommentsAndStrings(text))
		if i != line && (code == "" || strings.HasPrefix(code, "@") || strings.HasSuffix(code, "{") || strings.HasPrefix(code, "}")) {
			return exprPos{}
		}
		if first := firstWord(code); first != "" && dslSpec.ConstructByKeyword(first) != nil {
			// Another construct's header, or a spec still being declared.
			return exprPos{}
		}
	}
	return exprPos{}
}

// reachesCursor reports whether the expression opened on line header (1-based)
// runs down to the cursor line: every line after the header continues it.
func reachesCursor(lines []string, header, line int, cur string) bool {
	for k := header + 1; k <= line; k++ {
		prev := strings.TrimSpace(blankLineCommentsAndStrings(lines[k-2]))
		code := strings.TrimSpace(blankLineCommentsAndStrings(lineText(lines, k, line, cur)))
		depth := len(scanText(strings.Join(lines[header-1:k-1], "\n")).opens)
		if !continuesExpression(prev, code, depth) {
			return false
		}
	}
	return true
}

// continuesExpression reports whether a line continues the expression on the
// lines above it rather than starting a new clause or statement: a bracket
// opened above is still open, the line opens with an operator that needs a
// left operand, or the line above ended on one that needs a right operand.
// This is the rule the v1 rewriter joins lines by -- leading `&&`, `||`, `?`,
// `:` and a trailing operator -- widened to every binary operator.
func continuesExpression(prev, code string, openBrackets int) bool {
	if openBrackets > 0 {
		return true
	}
	for _, op := range []string{"&&", "||", "??", "?", ":", ".", ")", "]", "==", "!=", "<", ">", "+", "-", "*", "/", "%", "=>"} {
		if strings.HasPrefix(code, op) {
			return true
		}
	}
	if startsWithWord(code, "in") || startsWithWord(code, "startsWith") {
		return true
	}
	for _, op := range []string{"&&", "||", "??", "?", ":", "(", "[", ",", "=>", "==", "!=", "<", ">", "=", "+", "-", "*", "/", "%", "!"} {
		if strings.HasSuffix(prev, op) {
			return true
		}
	}
	return strings.HasSuffix(prev, " in") || strings.HasSuffix(prev, " startsWith")
}

// lineText returns line k of the source, except that the cursor line is only
// the text before the cursor.
func lineText(lines []string, k, cursorLine int, cur string) string {
	if k == cursorLine {
		return cur
	}
	return lines[k-1]
}

// triggerConcept returns the short name of the concept an automation fires on,
// read from the @trigger annotation in the preamble around line (1-based): the
// contiguous run of annotation and comment lines above and below it.
func triggerConcept(lines []string, line int) string {
	var preamble []string
	for i := line - 1; i >= 0 && i < len(lines); i-- {
		t := strings.TrimSpace(lines[i])
		if i != line-1 && !strings.HasPrefix(t, "@") && !strings.HasPrefix(t, "//") {
			break
		}
		preamble = append(preamble, t)
	}
	for i := line; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "@") && !strings.HasPrefix(t, "//") {
			break
		}
		preamble = append(preamble, t)
	}
	text := strings.Join(preamble, "\n")
	for _, re := range []*regexp.Regexp{triggerConceptKwarg, triggerEventConcept} {
		if m := re.FindStringSubmatch(text); m != nil {
			id := m[1]
			return id[strings.LastIndexByte(id, ':')+1:]
		}
	}
	return ""
}

// nestedLambdaParams returns the lambda parameters opened in expr and still in
// scope at its end. A lambda's body extends as far as it can (D9: `=>` binds
// loosest), so a parameter goes out of scope when the bracket its lambda sits
// in closes, or at a comma that ends its argument.
func nestedLambdaParams(expr string) []LambdaParam {
	toks := lexTokens(expr)
	var callees []string // one per open bracket: the call a `(` belongs to
	type scoped struct {
		p     LambdaParam
		depth int // the number of open brackets when the lambda began
	}
	var in []scoped
	drop := func(depth int) {
		for len(in) > 0 && in[len(in)-1].depth >= depth {
			in = in[:len(in)-1]
		}
	}
	callee := func() string {
		if len(callees) == 0 {
			return ""
		}
		return callees[len(callees)-1]
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.Type {
		case parser.TokenParenOpen:
			// `(a, b) =>` is a parameter list, not a group.
			if names, arrow, ok := lambdaParamList(toks, i); ok {
				for _, n := range names {
					in = append(in, scoped{LambdaParam{Name: n, Callee: callee()}, len(callees)})
				}
				i = arrow
				continue
			}
			name := ""
			if i > 0 && toks[i-1].Type == parser.TokenIdentifier {
				name = toks[i-1].Literal
			}
			callees = append(callees, name)
		case parser.TokenBracketOpen, parser.TokenBraceOpen:
			callees = append(callees, "")
		case parser.TokenParenClose, parser.TokenBracketClose, parser.TokenBraceClose:
			drop(len(callees))
			if len(callees) > 0 {
				callees = callees[:len(callees)-1]
			}
		case parser.TokenComma:
			drop(len(callees))
		case parser.TokenIdentifier:
			if i+1 < len(toks) && isArrow(toks[i+1]) && !strings.Contains(t.Literal, ".") {
				in = append(in, scoped{LambdaParam{Name: t.Literal, Callee: callee()}, len(callees)})
				i++
			}
		}
	}
	out := make([]LambdaParam, 0, len(in))
	for _, s := range in {
		out = append(out, s.p)
	}
	return out
}

// lambdaParamList recognises `( a, b ) =>` starting at toks[i] and returns the
// names and the index of the arrow.
func lambdaParamList(toks []parser.Token, i int) ([]string, int, bool) {
	var names []string
	j := i + 1
	for {
		if j >= len(toks) || toks[j].Type != parser.TokenIdentifier || strings.Contains(toks[j].Literal, ".") {
			return nil, 0, false
		}
		names = append(names, toks[j].Literal)
		j++
		if j < len(toks) && toks[j].Type == parser.TokenComma {
			j++
			continue
		}
		break
	}
	if j+1 >= len(toks) || toks[j].Type != parser.TokenParenClose || !isArrow(toks[j+1]) {
		return nil, 0, false
	}
	return names, j + 1, true
}

func isArrow(t parser.Token) bool {
	return t.Type == parser.TokenOperator && t.Literal == "=>"
}

// textScan is one pass over the text before the cursor that tracks what a
// token stream cannot: whether the cursor sits inside a string literal or a
// comment (the lexer returns nothing for an unterminated string), and which
// brackets are still open, with the call or annotation each `(` belongs to.
type textScan struct {
	inString    bool
	stringStart int // offset of the opening quote of the string the cursor is in
	inComment   bool
	opens       []openBracket
}

type openBracket struct {
	ch         byte
	offset     int
	annotation string // `@name(`: the annotation name
	callee     string // `name(` / `a.b.c(`: the called path
	kind       string // `mutation name(`: the word before the callee
}

func scanText(text string) textScan {
	var s textScan
	for i := 0; i < len(text); i++ {
		c := text[i]
		if s.inString {
			switch c {
			case '\\':
				i++
			case '"', '\n':
				// A newline also ends it: an authored string does not span
				// lines, and one unbalanced quote must not swallow the rest of
				// the file.
				s.inString = false
			}
			continue
		}
		switch {
		case c == '"':
			s.inString, s.stringStart = true, i
		case c == '/' && i+1 < len(text) && text[i+1] == '/':
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				s.inComment = true
				return s
			}
			i += end
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				s.inComment = true
				return s
			}
			i += end + 3
		case c == '(' || c == '[' || c == '{':
			ob := openBracket{ch: c, offset: i}
			if c == '(' {
				ob.annotation, ob.callee, ob.kind = callSiteBefore(text, i)
			}
			s.opens = append(s.opens, ob)
		case c == ')' || c == ']' || c == '}':
			if n := len(s.opens); n > 0 {
				s.opens = s.opens[:n-1]
			}
		}
	}
	return s
}

// callSiteBefore reads what precedes the `(` at offset i: an annotation name
// (`@filter(`), or a called path and the word before it (`mutation createTodo(`).
func callSiteBefore(text string, i int) (annotation, callee, kind string) {
	j := i
	for j > 0 && (isIdentChar(text[j-1]) || text[j-1] == '.') {
		j--
	}
	name := text[j:i]
	if name == "" {
		return "", "", ""
	}
	if j > 0 && text[j-1] == '@' {
		return name, "", ""
	}
	k := j
	for k > 0 && (text[k-1] == ' ' || text[k-1] == '\t') {
		k--
	}
	m := k
	for m > 0 && isIdentChar(text[m-1]) {
		m--
	}
	return "", name, text[m:k]
}

// outermostAnnotation returns the outermost still-open annotation argument
// list, so a call nested inside `@filter(row => lower(` still reads as the
// annotation's.
func (s textScan) outermostAnnotation() (openBracket, bool) {
	for _, ob := range s.opens {
		if ob.annotation != "" {
			return ob, true
		}
	}
	return openBracket{}, false
}

// insideConstructCall reports whether the cursor sits inside the argument list
// of a kind-prefixed call: `mutation createTodo(`, `logic decide(`.
func (s textScan) insideConstructCall() bool {
	for _, ob := range s.opens {
		if ob.ch == '(' && ob.callee != "" && invocationKindKeywords[ob.kind] {
			return true
		}
	}
	return false
}

// firstGroup returns the first non-empty of two submatch groups, given the
// index of each group's start in FindStringSubmatchIndex's pairs.
func firstGroup(text string, m []int, a, b int) string {
	if m[a] >= 0 {
		return text[m[a]:m[a+1]]
	}
	if m[b] >= 0 {
		return text[m[b]:m[b+1]]
	}
	return ""
}

// lineStartOffset returns the byte offset in text at which 1-based line n
// begins.
func lineStartOffset(text string, n int) int {
	off := 0
	for l := 1; l < n; l++ {
		idx := strings.IndexByte(text[off:], '\n')
		if idx < 0 {
			return len(text)
		}
		off += idx + 1
	}
	return off
}

// lineOfOffset returns the 1-based line of a byte offset in text.
func lineOfOffset(text string, off int) int {
	return strings.Count(text[:off], "\n") + 1
}

func startsWithWord(text, word string) bool {
	return dslclause.StartsWith(text, word)
}

func firstWord(text string) string {
	if i := strings.IndexFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '(' || r == '{' }); i >= 0 {
		return text[:i]
	}
	return text
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// positionPhrase names a position the way a legality line reads it: "Not
// allowed on the row in a query filter".
func positionPhrase(p tiers.Position) string {
	return positionPhrases[p]
}

var positionPhrases = map[tiers.Position]string{
	tiers.PositionQueryFilter:         "a query filter",
	tiers.PositionSort:                "a sort key",
	tiers.PositionSpecBody:            "a spec body",
	tiers.PositionRowAuthzArgument:    "a row-authz argument",
	tiers.PositionAutomationCondition: "an automation condition",
	tiers.PositionTriggerFilter:       "a trigger filter",
	tiers.PositionLogicBody:           "a logic body",
	tiers.PositionMutationValue:       "a mutation value",
	tiers.PositionStepArgument:        "a step argument",
	tiers.PositionToolDefault:         "a tool default",
	tiers.PositionPromptInput:         "a prompt input",
	tiers.PositionQueryRefine:         "a refine clause",
}

// isTraversal reports whether a catalog function is a relationship traversal,
// whose match lambda's parameter is a row.
func isTraversal(name string) bool {
	f, ok := functions.Lookup(name)
	return ok && f.Returns == functions.TypeRows
}
