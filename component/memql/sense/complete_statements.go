package sense

// complete_statements.go -- completion inside a statement body (edition 2026,
// epic memql#5370).
//
// A logic or an automation written in statements is a list of lines, each one
// statement. What may be typed at the start of one is: a statement keyword; a
// construct call by its kind (`query x(...)`), which is how a call is written
// -- a bare call is refused (body_call_kind_missing) -- so a construct named
// here is inserted WITH its kind; a name an earlier statement bound; and the
// roots every body reads (args, actor, now, config, and an automation's
// event). An argument is read `args.x`: the bare argument names a legacy
// automation body offers (G2) are not offered here, where they are refused.
//
// This is the set at a line's START, where no expression position applies.
// Inside a statement -- after `:=`, in a call's arguments, in a condition --
// the expression completer answers (complete_expr.go), and a statement logic
// body is its PositionLogicBody (exprpos.go). A body still written in the
// retired forms (a `step` block, a logic's `body { }`, the terse header) keeps
// the completion it had until the flip.

import (
	"regexp"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

var (
	statementHeader = regexp.MustCompile(`^[ \t]*(automation|logic)[ \t]+([A-Za-z_][A-Za-z0-9_]*)\b`)
	legacyBodyMark  = regexp.MustCompile(`(?m)^[ \t]*(?:step[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{|body[ \t]*\{)`)
	boundName       = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*:=`)
	loopVariable    = regexp.MustCompile(`^[ \t]*for[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+in\b`)
)

// inStatementBody reports whether the cursor (1-based line) sits in a logic
// or an automation body written in statements.
func inStatementBody(lines []string, line int, enc EnclosingConstruct) bool {
	_, ok := statementBody(strings.Join(lines, "\n"), line, enc)
	return ok
}

// statementBody reports whether the cursor sits directly in a logic or an
// automation body written in statements, and the construct's lines above the
// cursor's (its header first).
func statementBody(source string, line int, enc EnclosingConstruct) ([]string, bool) {
	if (enc.Keyword != "logic" && enc.Keyword != "automation") || len(enc.Blocks) > 0 || enc.Preamble {
		return nil, false
	}
	view := strings.Split(langparser.BlankCommentsAndStrings(source), "\n")
	lines := strings.Split(source, "\n")
	if line < 1 || line > len(view) {
		return nil, false
	}
	header := -1
	for k := line - 1; k >= 0; k-- {
		if m := statementHeader.FindStringSubmatch(view[k]); m != nil && m[1] == enc.Keyword && (enc.Name == "" || m[2] == enc.Name) {
			header = k
			break
		}
	}
	if header < 0 || header == line-1 || strings.Contains(view[header], "=>") {
		return nil, false // not found, the cursor on the header itself, or the terse header
	}
	// The whole construct decides whether it is written in statements: a step
	// block below the cursor makes it a legacy body as surely as one above.
	depth, opened := 0, false
	end := len(view) - 1
scan:
	for k := header; k < len(view); k++ {
		for _, c := range view[k] {
			switch c {
			case '{':
				depth++
				opened = true
			case '}':
				depth--
				if opened && depth == 0 {
					end = k
					break scan
				}
			}
		}
	}
	if legacyBodyMark.MatchString(strings.Join(view[header+1:end+1], "\n")) {
		return nil, false
	}
	return lines[header : line-1], true
}

// completeStatementBody is the completion at a line of a statement body.
func (s *Service) completeStatementBody(prefix string, enc EnclosingConstruct, construct []string) []CompletionItem {
	var items []CompletionItem
	// The construct's blocks -- args, an automation's precondition -- but never
	// the retired `body { }` wrapper or `step` block: the statements are the
	// body.
	for _, blk := range bodyBlocksForConstruct(enc) {
		if blk != "body" && blk != "step" && strings.HasPrefix(blk, prefix) {
			items = append(items, CompletionItem{
				Label: blk, Kind: "keyword", Detail: enc.Keyword + " block",
				Documentation: KeywordDocs[blk], InsertText: blk + " {", SortPriority: 2,
			}, blockSnippet(blk, enc.Keyword))
		}
	}
	keywords := append([]string{}, statementKeywords...)
	if enc.Keyword == "automation" {
		keywords = append(keywords, "publish")
	}
	for _, kw := range keywords {
		if strings.HasPrefix(kw, prefix) {
			items = append(items, CompletionItem{
				Label: kw, Kind: "keyword", Detail: "statement",
				Documentation: KeywordDocs[kw], InsertText: kw, SortPriority: 2,
			})
		}
	}

	// The call kinds, then each construct by its kind.
	for _, kw := range invocationKeywordsForConstruct(enc) {
		if strings.HasPrefix(kw, prefix) {
			items = append(items, CompletionItem{
				Label: kw, Kind: "keyword", Detail: "construct call",
				InsertText: kw + " ", SortPriority: 3,
			})
		}
	}
	if s.registries != nil {
		for _, name := range s.registries.FunctionNames() {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			fn, ok := s.registries.FunctionGet(name)
			kind := statementCallKind(fn.Kind)
			if !ok || kind == "" || (enc.Keyword == "logic" && (kind == "automation" || kind == "action")) {
				continue
			}
			items = append(items, CompletionItem{
				Label: name, Kind: "function", Detail: kind,
				Documentation: fn.Description, InsertText: kind + " " + name + "(",
				SortPriority: 4,
			})
		}
	}

	// Names the statements above bound, and the roots.
	seen := map[string]bool{}
	for _, l := range construct[1:] {
		for _, re := range []*regexp.Regexp{boundName, loopVariable} {
			if m := re.FindStringSubmatch(l); m != nil && !seen[m[1]] && strings.HasPrefix(m[1], prefix) {
				seen[m[1]] = true
				items = append(items, CompletionItem{
					Label: m[1], Kind: "variable", Detail: "bound above",
					InsertText: m[1], SortPriority: 1,
				})
			}
		}
	}
	roots := []string{"args", "actor", "now", "config"}
	if enc.Keyword == "automation" {
		roots = append(roots, "event")
	}
	for _, r := range roots {
		if strings.HasPrefix(r, prefix) && !seen[r] {
			items = append(items, CompletionItem{
				Label: r, Kind: "keyword", Detail: "root",
				Documentation: KeywordDocs[r], InsertText: r, SortPriority: 5,
			})
		}
	}

	// The expression functions.
	for name, def := range BuiltinFunctions {
		if strings.HasPrefix(name, prefix) {
			items = append(items, CompletionItem{
				Label: name, Kind: "builtin", Detail: def.Signature,
				Documentation: def.Doc, InsertText: name + "(", SortPriority: 6,
			})
		}
	}
	return items
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
