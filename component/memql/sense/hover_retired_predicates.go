package sense

// hover_retired_predicates.go -- hover over the predicate positions' legacy
// spellings (D1): a query filter with no lambda header, a spec or trait with a
// `{ return ... }` body, an @filter whose argument is not a lambda. They are
// the retired forms an author meets most, since every construct the tree has
// not migrated is one, so their card shows the author's own construct as
// `memqlmigrate --rewrite=expressions` would write it: the same rewrite, run
// over the construct under the cursor. Where the rewrite refuses the
// construct, the card falls back to the parser's table.

import (
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The parser's rule ids for the four predicate-position forms.
const (
	ruleFilterWithoutLambda = "retired_filter_without_lambda"
	ruleSpecReturnBody      = "retired_spec_return_body"
	ruleTraitReturnBody     = "retired_trait_return_body"
	ruleFilterAnnotation    = "retired_filter_annotation"
)

// retiredPredicateAt recognises a legacy predicate position at the token
// under the cursor:
//
//   - the `filter` keyword of a query filter with no lambda header;
//   - the `return` of a spec or trait written `{ return ... }`, or the `spec`
//     or `trait` keyword of its header;
//   - `@filter` (the `@` or the name) when its argument is not a lambda.
//
// example is the construct migrated, or "" when the rewrite refuses it.
func (s *Service) retiredPredicateAt(source string, toks []parser.Token, idx int) (form parser.RetiredForm, example string, ok bool) {
	src := []rune(source)
	t := toks[idx]
	switch {
	case annotationNameAt(toks, idx, "filter") >= 0:
		name := annotationNameAt(toks, idx, "filter")
		open := name + 1
		if open >= len(toks) || toks[open].Type != parser.TokenParenOpen || lambdaHeaderAt(toks, open+1) {
			return form, "", false
		}
		if form, ok = retiredForm(ruleFilterAnnotation); !ok {
			return form, "", false
		}
		if end := matchingClose(toks, open); end > 0 {
			text := string(src[toks[name-1].Pos:toks[end].EndPos])
			example = strings.TrimSpace(s.migrate(source, text, nil))
		}
		return form, example, true

	case t.Type == parser.TokenIdentifier && t.Literal == "filter" && startsLine(toks, idx):
		// A clause with nothing after the keyword yet is mid-typing, not a form.
		if idx+1 >= len(toks) || toks[idx+1].Line != t.Line || lambdaHeaderAt(toks, idx+1) {
			return form, "", false
		}
		brace := innermostOpenBrace(toks, idx)
		if brace < 0 || firstWordOfLine(toks, brace) != "query" {
			return form, "", false
		}
		if form, ok = retiredForm(ruleFilterWithoutLambda); !ok {
			return form, "", false
		}
		if out := s.migrate(source, constructText(src, toks, brace), nil); out != "" {
			example = filterClauseOf(out)
		}
		return form, example, true

	case t.Type == parser.TokenKeywordReturn:
		brace := innermostOpenBrace(toks, idx)
		if brace < 0 {
			return form, "", false
		}
		return s.retiredBody(source, src, toks, lineStartIndex(toks, brace))

	case t.Type == parser.TokenIdentifier && (t.Literal == "spec" || t.Literal == "trait") && startsLine(toks, idx):
		return s.retiredBody(source, src, toks, idx)
	}
	return form, "", false
}

// retiredBody answers for a spec or trait declaration whose header starts at
// toks[head]: `spec <bound> <name> {` or `trait <name> {`. A declaration in
// the v1 form (`= row => ...`) is not retired.
func (s *Service) retiredBody(source string, src []rune, toks []parser.Token, head int) (form parser.RetiredForm, example string, ok bool) {
	h := toks[head]
	if h.Type != parser.TokenIdentifier {
		return form, "", false
	}
	var rule, name, bound string
	brace := -1
	switch h.Literal {
	case "spec":
		if head+3 < len(toks) && isName(toks[head+1]) && isName(toks[head+2]) && toks[head+3].Type == parser.TokenBraceOpen {
			rule, bound, name, brace = ruleSpecReturnBody, toks[head+1].Literal, toks[head+2].Literal, head+3
		}
	case "trait":
		if head+2 < len(toks) && isName(toks[head+1]) && toks[head+2].Type == parser.TokenBraceOpen {
			rule, name, brace = ruleTraitReturnBody, toks[head+1].Literal, head+2
		}
	}
	if brace < 0 {
		return form, "", false
	}
	if form, ok = retiredForm(rule); !ok {
		return form, "", false
	}
	// The declaration's own receiver: an @actor-bound spec reads the actor.
	own := map[string]parser.PredicateInfo{name: {Actor: s.isActorBinding(source, name, bound)}}
	if out := s.migrate(source, constructText(src, toks, brace), own); out != "" {
		example = strings.TrimSpace(out)
	}
	return form, example, true
}

// migrate runs the expressions rewrite over text, one construct cut out of
// source, and returns what it writes, or "" when it refuses the construct or
// leaves it unchanged. own overrides the predicates the rewrite is told about.
func (s *Service) migrate(source, text string, own map[string]parser.PredicateInfo) string {
	preds := s.rewritePredicates(source)
	for name, info := range own {
		preds[name] = info
	}
	out, err := parser.RewriteExpressions([]byte(text), preds)
	if err != nil || string(out) == text {
		return ""
	}
	return string(out)
}

// rewritePredicates is what the rewrite needs to know about every spec and
// trait a construct may name: whether it is applied to a row or to the actor.
// The registry answers for the loaded tree, and the document's own
// declarations for anything not loaded yet.
func (s *Service) rewritePredicates(source string) map[string]parser.PredicateInfo {
	preds := map[string]parser.PredicateInfo{}
	if doc, err := parser.CollectPredicates(map[string][]byte{"document.memql": []byte(source)}); err == nil {
		for name, info := range doc {
			preds[name] = info
		}
	}
	if s.registries != nil {
		for _, name := range s.registries.SpecNames() {
			if info, ok := s.registries.SpecGet(name); ok && info != nil {
				preds[name] = parser.PredicateInfo{Actor: info.Kind == "context"}
			}
		}
	}
	return preds
}

// isActorBinding reports whether a spec is a predicate over the actor: the
// registry says so for a loaded spec, and for an unloaded one its binding is
// an @actor shape when a loaded context spec binds the same shape, or when the
// document declares it so.
func (s *Service) isActorBinding(source, name, bound string) bool {
	if s.registries != nil {
		if info, ok := s.registries.SpecGet(name); ok && info != nil {
			return info.Kind == "context"
		}
		for _, other := range s.registries.SpecNames() {
			if info, ok := s.registries.SpecGet(other); ok && info != nil && info.Kind == "context" && info.Bound == bound {
				return true
			}
		}
	}
	doc, err := parser.CollectPredicates(map[string][]byte{"document.memql": []byte(source)})
	return err == nil && doc[name].Actor
}

// annotationNameAt returns the index of the name token of `@<name>` when the
// token at idx is its `@` or its name, else -1.
func annotationNameAt(toks []parser.Token, idx int, name string) int {
	t := toks[idx]
	switch {
	case t.Type == parser.TokenAt && idx+1 < len(toks) && toks[idx+1].Literal == name && adjacent(t, toks[idx+1]):
		return idx + 1
	case t.Type == parser.TokenIdentifier && t.Literal == name && idx > 0 && toks[idx-1].Type == parser.TokenAt && adjacent(toks[idx-1], t):
		return idx
	}
	return -1
}

// lambdaHeaderAt reports whether toks[i] opens a lambda: `x =>` or
// `(x, y) =>`.
func lambdaHeaderAt(toks []parser.Token, i int) bool {
	if i >= len(toks) {
		return false
	}
	if toks[i].Type == parser.TokenIdentifier {
		return i+1 < len(toks) && isArrow(toks[i+1]) && !strings.Contains(toks[i].Literal, ".")
	}
	if toks[i].Type == parser.TokenParenOpen {
		_, _, ok := lambdaParamList(toks, i)
		return ok
	}
	return false
}

// isName reports whether a token is one plain identifier.
func isName(t parser.Token) bool {
	return t.Type == parser.TokenIdentifier && !strings.ContainsAny(t.Literal, ".:")
}

// startsLine reports whether toks[idx] is the first token on its line.
func startsLine(toks []parser.Token, idx int) bool {
	return idx == 0 || toks[idx-1].Line != toks[idx].Line
}

// lineStartIndex returns the index of the first token on toks[idx]'s line.
func lineStartIndex(toks []parser.Token, idx int) int {
	for idx > 0 && toks[idx-1].Line == toks[idx].Line {
		idx--
	}
	return idx
}

// innermostOpenBrace returns the index of the innermost `{` still open at
// toks[idx], or -1.
func innermostOpenBrace(toks []parser.Token, idx int) int {
	depth := 0
	for i := idx - 1; i >= 0; i-- {
		switch toks[i].Type {
		case parser.TokenBraceClose:
			depth++
		case parser.TokenBraceOpen:
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// matchingClose returns the index of the bracket that closes the one at
// toks[open], or -1 when the source ends first.
func matchingClose(toks []parser.Token, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch toks[i].Type {
		case parser.TokenParenOpen, parser.TokenBracketOpen, parser.TokenBraceOpen:
			depth++
		case parser.TokenParenClose, parser.TokenBracketClose, parser.TokenBraceClose:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// constructText cuts one construct out of the source: from the start of the
// line its opening brace (toks[brace]) is on to the brace that closes it, or
// to the end of the source when nothing does yet.
func constructText(src []rune, toks []parser.Token, brace int) string {
	start := toks[brace].Pos
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	end := len(src)
	if c := matchingClose(toks, brace); c > 0 {
		end = toks[c].EndPos
	}
	return string(src[start:end])
}

// filterClauseOf returns the filter clause of a migrated query: its `filter`
// line and the continuation lines under it, dedented.
func filterClauseOf(query string) string {
	lines := strings.Split(query, "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if !startsWithWord(trimmed, "filter") {
			continue
		}
		indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		out := []string{trimmed}
		for _, next := range lines[i+1:] {
			nt := strings.TrimSpace(next)
			if !continuesClause(nt) {
				break
			}
			out = append(out, strings.TrimPrefix(next, indent))
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// continuesClause reports whether a line continues the clause above it: it
// opens with a binary operator or closes a bracket.
func continuesClause(line string) bool {
	for _, op := range []string{"&&", "||", "??", "?", ":", ")", "]"} {
		if strings.HasPrefix(line, op) {
			return true
		}
	}
	return false
}
