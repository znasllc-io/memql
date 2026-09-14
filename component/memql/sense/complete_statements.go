package sense

// complete_statements.go -- completion inside a body written in statements
// (edition 2026, epic memql#5370).
//
// A logic or an automation written in statements is a list of lines, each one
// statement, and this completer answers where the statement language rather
// than an expression decides what may be written:
//
//   - Where a statement starts: a word that opens one
//     (parser.BodyStatementKeywords -- publish in an automation only, D14, and
//     no return in a parallel branch); a construct call by its kind -- a bare
//     call is refused (body_call_kind_missing), so a construct named here is
//     inserted WITH its kind; and, before the first statement, the
//     construct's blocks: args first, then an automation's named
//     preconditions. Directly in a switch a line opens a case or the default,
//     and directly in a parallel a branch. Nothing else starts a statement --
//     a root, a name bound above or a function at a line's start is refused --
//     so none is offered there.
//   - After a statement, on its line: what may follow it, in the order the
//     parser takes it -- `else` after an if's block, `wait any` after a
//     parallel's, and `on surface(...)`, `retry(n)` and `on error continue`
//     after the statements that take them.
//
// Everywhere else -- after `:=`, in a call's arguments, in a condition, on a
// line that continues an expression -- an expression is being written: the
// expression completer answers (complete_expr.go), and a statement logic body
// is its PositionLogicBody (exprpos.go). A body still written in the retired
// forms (a `step` block, a logic's `body { }`, the terse header) keeps the
// completion it had until the flip.

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

var (
	statementHeader = regexp.MustCompile(`^[ \t]*(automation|logic)[ \t]+([A-Za-z_][A-Za-z0-9_]*)\b`)
	legacyBodyMark  = regexp.MustCompile(`(?m)^[ \t]*(?:step[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{|body[ \t]*\{)`)
	// leadingWord is a line's first word, past a `}` closing the block above
	// it (`} else {`).
	leadingWord = regexp.MustCompile(`^[ \t]*(?:\}[ \t]*)?([A-Za-z_][A-Za-z0-9_]*)`)
	// callStatementHead is a line holding a construct call statement -- `<kind>
	// f(`, `x := <kind> f(`, `return <kind> f(` -- with the return and the kind.
	callStatementHead = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*[ \t]*:=[ \t]*|(return)[ \t]+)?([a-z]+)[ \t]+[A-Za-z_][A-Za-z0-9_.]*[ \t]*\(`)
	// statementValueStart is a line whose statement's whole value starts at
	// the cursor -- `x := `, `return ` -- where a construct call may be written.
	statementValueStart = regexp.MustCompile(`^[ \t]*(?:[A-Za-z_][A-Za-z0-9_]*[ \t]*:=|return)[ \t]*$`)
)

// statementBlockWords are the words whose `{` opens a list of statements --
// or, for a switch and a parallel, of cases and branches. Any other brace in a
// body is an expression's: a map, a publish payload.
var statementBlockWords = map[string]bool{
	"if": true, "else": true, "for": true, "switch": true, "case": true, "default": true, "parallel": true, "branch": true,
}

// statementSite is where the cursor sits in a body written in statements.
type statementSite struct {
	// opener is what opened the innermost block holding the cursor: "" when it
	// sits directly in the construct, a statementBlockWords word in a
	// statement's block, and "=" in a brace an expression opened.
	opener string
	// inBranch: a parallel branch encloses the cursor, and a branch cannot
	// return.
	inBranch bool
	// continued: the cursor is inside an open ( or [, or its line continues
	// the expression of the line above -- no statement starts there.
	continued bool
	// args, preconditions and started say what precedes the cursor directly in
	// the construct: its args block, a precondition block, a statement.
	args, preconditions, started bool
	// head is the code on the cursor's line before the word being typed, with
	// strings and comments blanked.
	head string
	// closed is the opener of the block the last `}` in head closes.
	closed string
}

// inStatementBody reports whether the cursor -- on 1-based line, after cur,
// the text before it on that line -- sits in a logic or an automation body
// written in statements, outside its args and precondition blocks.
func inStatementBody(lines []string, line int, cur string, enc EnclosingConstruct) bool {
	_, ok := statementBodySite(strings.Join(lines, "\n"), line, cur, enc, "")
	return ok
}

// cursorLine is the text before the cursor on its line.
func cursorLine(source string, line, col int) string {
	before := textBeforeCursor(source, line, col)
	return before[strings.LastIndexByte(before, '\n')+1:]
}

// statementBodySite reports whether the cursor sits in a logic or an
// automation body written in statements, outside its args and precondition
// blocks, and where. cur is the text before the cursor on its line, and prefix
// the word being typed at its end.
func statementBodySite(source string, line int, cur string, enc EnclosingConstruct, prefix string) (statementSite, bool) {
	var site statementSite
	if (enc.Keyword != "logic" && enc.Keyword != "automation") || enc.Preamble {
		return site, false
	}
	view := strings.Split(langparser.BlankCommentsAndStrings(source), "\n")
	if line < 1 || line > len(view) {
		return site, false
	}
	header := -1
	for k := line - 1; k >= 0; k-- {
		if m := statementHeader.FindStringSubmatch(view[k]); m != nil && m[1] == enc.Keyword && (enc.Name == "" || m[2] == enc.Name) {
			header = k
			break
		}
	}
	if header < 0 || header == line-1 || strings.Contains(view[header], "=>") {
		return site, false // not found, the cursor on the header itself, or the terse header
	}
	// The whole construct decides whether it is written in statements: a step
	// block below the cursor makes it a legacy body as surely as one above.
	if legacyBodyMark.MatchString(strings.Join(view[header+1:constructEnd(view, header)+1], "\n")) {
		return site, false
	}

	// Walk the construct to the cursor, keeping what opened each brace still
	// open and how many parentheses and brackets are.
	cut := len(cur)
	if cut > len(view[line-1]) {
		cut = len(view[line-1])
	}
	var stack []string
	depth, prev := 0, ""
	for k := header; k < line; k++ {
		code := view[k]
		if k == line-1 {
			code = code[:cut]
		}
		word := ""
		if m := leadingWord.FindStringSubmatch(code); m != nil {
			word = m[1]
		}
		// A line above the cursor's, directly in the construct and not an
		// expression's continuation, is one of its items.
		if k > header && k < line-1 && len(stack) == 1 && depth == 0 && word != "" &&
			!continuesExpression(strings.TrimSpace(prev), strings.TrimSpace(code), 0) {
			switch word {
			case "args":
				site.args = true
			case "precondition":
				site.preconditions = true
			default:
				site.started = true
			}
		}
		site.closed = ""
		for _, c := range code {
			switch c {
			case '{':
				opener := "="
				switch {
				case len(stack) == 0:
					opener = enc.Keyword
				case statementBlockWords[word] || word == "args" || word == "precondition":
					opener = word
				}
				stack = append(stack, opener)
			case '}':
				if len(stack) > 0 {
					site.closed = stack[len(stack)-1]
					stack = stack[:len(stack)-1]
				}
			case '(', '[':
				depth++
			case ')', ']':
				if depth > 0 {
					depth--
				}
			}
		}
		if k < line-1 && strings.TrimSpace(code) != "" {
			prev = code
		}
	}
	if len(stack) == 0 {
		return site, false // past the construct's closing brace
	}
	for _, o := range stack[1:] {
		switch o {
		case "args", "precondition":
			return site, false // a block of the construct's own: its completion is the block's
		case "branch":
			site.inBranch = true
		}
	}
	if len(stack) > 1 {
		site.opener = stack[len(stack)-1]
	}
	site.head = strings.TrimSpace(strings.TrimSuffix(view[line-1][:cut], prefix))
	site.continued = depth > 0 || (site.head == "" && continuesExpression(strings.TrimSpace(prev), "", 0))
	return site, true
}

// constructEnd is the line holding the brace that closes the construct whose
// header is on line header, or the last line when it is not closed.
func constructEnd(view []string, header int) int {
	depth, opened := 0, false
	for k := header; k < len(view); k++ {
		for _, c := range view[k] {
			switch c {
			case '{':
				depth++
				opened = true
			case '}':
				depth--
				if opened && depth == 0 {
					return k
				}
			}
		}
	}
	return len(view) - 1
}

// completeAtStatement answers where the statement language decides what is
// written -- a statement's start, and what follows a statement on its line --
// and reports whether it did.
func (s *Service) completeAtStatement(ctx CursorContext, source string, line, col int) ([]CompletionItem, bool) {
	if ctx.Kind != ContextFuncBody {
		return nil, false
	}
	site, ok := statementBodySite(source, line, cursorLine(source, line, col), ctx.Enclosing, ctx.Prefix)
	if !ok || site.continued {
		return nil, false
	}
	if site.head == "" {
		return s.statementStartItems(ctx.Prefix, ctx.Enclosing, site), true
	}
	words, ok := trailingWords(site)
	if !ok {
		return nil, false
	}
	var items []CompletionItem
	for _, w := range words {
		if strings.HasPrefix(w, ctx.Prefix) {
			items = append(items, CompletionItem{
				Label: w, Kind: "keyword", Detail: "trailing clause",
				Documentation: KeywordDocs[w], InsertText: w, SortPriority: 1,
			})
		}
	}
	return items, true
}

// statementStartItems is what may be written where a statement starts.
func (s *Service) statementStartItems(prefix string, enc EnclosingConstruct, site statementSite) []CompletionItem {
	var items []CompletionItem
	keyword := func(kw, detail string, priority int) {
		if strings.HasPrefix(kw, prefix) {
			items = append(items, CompletionItem{
				Label: kw, Kind: "keyword", Detail: detail,
				Documentation: KeywordDocs[kw], InsertText: kw, SortPriority: priority,
			})
		}
	}
	switch site.opener {
	case "switch":
		keyword("case", "switch branch", 1)
		keyword("default", "switch branch", 1)
		return items
	case "parallel":
		keyword("branch", "parallel branch", 1)
		return items
	case "=":
		return nil // a map's or a payload's key
	}

	// Before the first statement, directly in the construct: its blocks, args
	// first, then an automation's preconditions -- never the retired `body { }`
	// wrapper or `step` block, since the statements are the body.
	if site.opener == "" && !site.started {
		for _, blk := range bodyBlocksForConstruct(enc) {
			if (blk != "args" && blk != "precondition") || (blk == "args" && (site.args || site.preconditions)) || !strings.HasPrefix(blk, prefix) {
				continue
			}
			item := CompletionItem{
				Label: blk, Kind: "keyword", Detail: enc.Keyword + " block",
				Documentation: KeywordDocs[blk], InsertText: blk + " {", SortPriority: 1,
			}
			snippet := blockSnippet(blk, enc.Keyword)
			if langparser.IsNamedBlock(blk) {
				item.InsertText = blk + " "
				snippet = namedBlockSnippet(blk, enc.Keyword)
			}
			items = append(items, item, snippet)
		}
	}

	for _, kw := range langparser.BodyStatementKeywords() {
		if (kw == "publish" && enc.Keyword != "automation") || (kw == "return" && site.inBranch) {
			continue // a logic may not publish (D14), and a parallel branch cannot return
		}
		keyword(kw, "statement", 2)
	}

	return append(items, s.statementCallItems(prefix, enc)...)
}

// statementCallItems is a construct call where a statement may write one: each
// kind a statement calls, then each registered construct inserted with the
// kind it is called by -- in a logic, only what a logic may call (D14).
func (s *Service) statementCallItems(prefix string, enc EnclosingConstruct) []CompletionItem {
	var items []CompletionItem
	for _, kind := range langparser.BodyCallKinds() {
		if enc.Keyword == "logic" && !compiler.LogicMayCall(kind) {
			continue
		}
		if strings.HasPrefix(kind, prefix) {
			items = append(items, CompletionItem{
				Label: kind, Kind: "keyword", Detail: "construct call",
				InsertText: kind + " ", SortPriority: 3,
			})
		}
	}
	if s.registries == nil {
		return items
	}
	for _, name := range s.registries.FunctionNames() {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		fn, ok := s.registries.FunctionGet(name)
		if !ok {
			continue
		}
		kind := statementCallKind(fn.Kind)
		if kind == "" || (enc.Keyword == "logic" && !compiler.LogicMayCall(kind)) {
			continue
		}
		items = append(items, CompletionItem{
			Label: name, Kind: "function", Detail: kind,
			Documentation: fn.Description, InsertText: kind + " " + name + "(",
			SortPriority: 4,
		})
	}
	return items
}

// trailingWords is what may follow a statement on its line, given the code
// before the word being typed, and whether the line holds a statement that
// takes something after it: `else` after an if's block, `wait any` after a
// parallel's, and the clauses a call, a for or a parallel takes, in the order
// the parser takes them -- `on surface(...)` (an action), `retry(n)` (a
// call), `on error continue` (a call, a for or a parallel; never a return).
func trailingWords(site statementSite) ([]string, bool) {
	head := strings.Join(strings.Fields(site.head), " ")
	if strings.HasPrefix(head, "}") {
		tail := strings.TrimSpace(head[1:])
		switch site.closed {
		case "if", "else":
			switch tail {
			case "":
				return []string{"else"}, true
			case "else":
				return []string{"if"}, true
			}
		case "for":
			return onErrorNext(tail), true
		case "parallel":
			switch {
			case tail == "":
				return []string{"wait", "on"}, true
			case tail == "wait":
				return []string{"any"}, true
			case strings.HasPrefix(tail, "wait any"):
				return onErrorNext(strings.TrimSpace(strings.TrimPrefix(tail, "wait any"))), true
			}
			return onErrorNext(tail), true
		}
		return nil, true
	}

	m := callStatementHead.FindStringSubmatchIndex(head)
	if m == nil {
		return nil, false
	}
	isReturn, kind := m[2] >= 0, head[m[4]:m[5]]
	closeAt := matchingParen(head, m[1]-1)
	if closeAt < 0 || !bodyCallKind(kind) {
		return nil, false
	}
	tail := strings.TrimSpace(head[closeAt+1:])
	surface := kind == "action" && !strings.Contains(tail, "on surface(") && !strings.Contains(tail, "retry(")
	retry := !strings.Contains(tail, "retry(") && !strings.Contains(tail, "on error")
	onError := !isReturn && !strings.Contains(tail, "on error")
	switch {
	case strings.HasSuffix(tail, "on error"):
		return []string{"continue"}, true
	case tail == "on" || strings.HasSuffix(tail, " on"):
		var words []string
		if surface {
			words = append(words, "surface")
		}
		if onError {
			words = append(words, "error")
		}
		return words, true
	case tail == "" || strings.HasSuffix(tail, ")"):
		var words []string
		if retry {
			words = append(words, "retry")
		}
		if surface || onError {
			words = append(words, "on")
		}
		return words, true
	}
	return nil, true
}

// onErrorNext is what follows tail in `on error continue`, the one clause a
// for takes.
func onErrorNext(tail string) []string {
	switch tail {
	case "":
		return []string{"on"}
	case "on":
		return []string{"error"}
	case "on error":
		return []string{"continue"}
	}
	return nil
}

// matchingParen is the index of the `)` closing the `(` at open, or -1.
func matchingParen(code string, open int) int {
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// bodyCallKind reports whether a word is a construct kind a statement calls.
func bodyCallKind(word string) bool {
	for _, k := range langparser.BodyCallKinds() {
		if k == word {
			return true
		}
	}
	return false
}

// statementExpressionItems is the completion of an expression in a body
// written in statements that no expression position claimed -- an automation's
// return value, a publish payload's value: the expression completer's set at
// the position a statement's value is evaluated in.
func (s *Service) statementExpressionItems(ctx CursorContext, source string, line, col int) []CompletionItem {
	ctx.Position = tiers.PositionStepArgument
	if ctx.Enclosing.Keyword == "logic" {
		ctx.Position = tiers.PositionLogicBody
	}
	return s.completeExpression(ctx, source, line, col)
}

// statementCallKind is the kind word a statement calls a registered function
// with: the function's own kind, or "" for one no statement calls by name.
func statementCallKind(kind string) string {
	switch k := strings.ToLower(strings.TrimSpace(kind)); k {
	case "query", "logic", "builtin", "automation", "action":
		return k
	case "mutation", "mutate":
		return "mutation"
	}
	return ""
}
