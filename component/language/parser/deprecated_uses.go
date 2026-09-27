package parser

// deprecated_uses.go -- where a source spells a deprecated language form, and
// the parser's refusal of one whose window has closed (memql#5390).
//
// WHAT a deprecated form is, and how long its window runs, is
// component/language/deprecation. This file answers the two questions only the
// front end can:
//
//   - WHERE a source spells one. ScanDeprecatedUses is read by the engine's
//     load, by Sense and by the language server, so the warning, the count and
//     the squiggle agree about every use rather than each finding its own.
//   - WHAT the parser does with one: nothing while the form is inside its
//     window -- it parses exactly as its replacement does -- and, once the
//     window is spent, a refusal shaped like a retired form's (a
//     *RetiredFormError carrying the form's rule), so every caller that already
//     reads a retired form's code reads this one's.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

// ruleDeprecatedArrayType is the rule parseTypeRef consults for `array(T)`.
const ruleDeprecatedArrayType = deprecation.ArrayType

// ruleDeprecatedAllowedRoles is the rule parseAttributeArgs consults for
// `@allowedRoles(...)` (memql#5438).
const ruleDeprecatedAllowedRoles = deprecation.AllowedRoles

// allowedRolesName is the annotation the AllowedRoles form is written as.
const allowedRolesName = "allowedRoles"

// DeprecatedUse is one place a source spells a deprecated form.
type DeprecatedUse struct {
	// Rule is the form's rule (deprecation.Form.Rule).
	Rule string
	// Line and Column are where the spelling starts in the author's source,
	// 1-based; the column counts runes, as every Sense position does.
	Line, Column int
	// Text is the spelling as written, e.g. "array(string)" or
	// `@allowedRoles("assistant")`.
	Text string

	// besides names, for an annotation form, the OTHER annotations written in
	// the same run above the construct, sorted and comma-joined: what a
	// replacement must not collide with. A string rather than a slice so a
	// use stays comparable.
	besides string
}

// Replacement is what to write over the use: the form's replacement with this
// use's own parts in it -- `array(string)` is `[]string`, and
// `@allowedRoles("assistant")` is `@requiresAgentRole("assistant")`. ok is
// false for a use of a form this scanner does not spell, one with nothing to
// carry across, and an @allowedRoles list vocabulary cannot carry across
// exactly (AllowedRolesReplacement says why); vocabulary may be nil, which
// leaves every @allowedRoles use without one.
func (u DeprecatedUse) Replacement(vocabulary *RoleVocabulary) (text string, ok bool) {
	switch u.Rule {
	case ruleDeprecatedArrayType:
		rest, found := strings.CutPrefix(u.Text, "array")
		open := strings.Index(rest, "(")
		if !found || open < 0 || !strings.HasSuffix(rest, ")") || strings.TrimSpace(rest[:open]) != "" {
			return "", false
		}
		element := strings.TrimSpace(rest[open+1 : len(rest)-1])
		if element == "" {
			return "", false
		}
		return "[]" + element, true
	case ruleDeprecatedAllowedRoles:
		decision := u.AllowedRolesReplacement(vocabulary)
		return decision.Annotation, decision.Annotation != ""
	}
	return "", false
}

// AllowedRolesReplacement decides an @allowedRoles use: the annotation that
// replaces it, or why it is left for its author (RoleVocabulary.
// RewriteAllowedRoles). On top of that decision it leaves a use whose
// replacement the construct already carries -- a second @requiresRank or
// @requiresAgentRole would be refused as written twice -- and one whose list
// is not the quoted strings the annotation takes.
func (u DeprecatedUse) AllowedRolesReplacement(vocabulary *RoleVocabulary) AllowedRolesRewrite {
	if u.Rule != ruleDeprecatedAllowedRoles {
		return AllowedRolesRewrite{Reason: "not an @allowedRoles use"}
	}
	values, ok := allowedRolesValues(u.Text)
	if !ok {
		return AllowedRolesRewrite{Reason: "the list is not written as quoted strings, which @allowedRoles takes"}
	}
	decision := vocabulary.RewriteAllowedRoles(values)
	if decision.Annotation == "" {
		return decision
	}
	name := strings.TrimPrefix(decision.Annotation[:strings.Index(decision.Annotation, "(")], "@")
	for _, other := range strings.Split(u.besides, ",") {
		if other == name {
			return AllowedRolesRewrite{Reason: "the construct already carries @" + name + ", so " +
				decision.Annotation + " beside it would be written twice; merge the two by hand"}
		}
	}
	return u.carryComments(decision)
}

// carryComments keeps the comments written inside the use's argument list in
// the annotation that replaces it. A rewrite replaces the whole spelling, and
// the lexer hands back no comments, so a list laid out over several lines with
// a note beside a role lost the note (memql#5438).
//
//   - An agent list keeps its values, so its argument list is kept exactly as
//     written -- layout, comments and all -- under the new name.
//   - A person list becomes one rung, and a note about a role it no longer
//     names still belongs with the gate: each comment is carried inside the
//     new argument list as a block comment, in order. A line comment whose
//     text would end that block early is not carried across: the use is left
//     for its author, with the reason, like every list the rewrite cannot
//     carry exactly.
func (u DeprecatedUse) carryComments(decision AllowedRolesRewrite) AllowedRolesRewrite {
	comments := commentsIn(u.Text)
	if len(comments) == 0 {
		return decision
	}
	if strings.HasPrefix(decision.Annotation, "@requiresAgentRole(") && strings.HasPrefix(u.Text, "@"+allowedRolesName+"(") {
		decision.Annotation = "@requiresAgentRole" + strings.TrimPrefix(u.Text, "@"+allowedRolesName)
		return decision
	}
	carried := make([]string, 0, len(comments))
	for _, c := range comments {
		if !strings.HasPrefix(c, "//") {
			carried = append(carried, c) // a block comment, whole
			continue
		}
		body := strings.TrimSpace(strings.TrimLeft(c, "/"))
		if strings.Contains(body, "*/") {
			return AllowedRolesRewrite{Reason: fmt.Sprintf(
				"the comment %q inside the list cannot be carried into %s as a block comment; rewrite it by hand",
				c, decision.Annotation)}
		}
		carried = append(carried, "/* "+body+" */")
	}
	closeAt := strings.LastIndex(decision.Annotation, ")")
	decision.Annotation = decision.Annotation[:closeAt] + " " + strings.Join(carried, " ") + ")"
	return decision
}

// commentsIn returns the comments written between the tokens of text, in
// order: a line comment to the end of its line, a block comment whole. The
// lexer drops both, so the gaps between consecutive tokens hold nothing but
// whitespace and comments -- which is also why a string's contents can never
// be mistaken for one.
func commentsIn(text string) []string {
	tokens, err := NewLexer(text).Tokenize()
	if err != nil {
		return nil
	}
	runes := []rune(text)
	var out []string
	gap := func(from, to int) {
		if from < 0 {
			from = 0
		}
		if to > len(runes) {
			to = len(runes)
		}
		for i := from; i < to; {
			if runes[i] != '/' || i+1 >= to || (runes[i+1] != '/' && runes[i+1] != '*') {
				i++
				continue
			}
			j := i + 2
			if runes[i+1] == '/' {
				for j < to && runes[j] != '\n' {
					j++
				}
				out = append(out, strings.TrimRight(string(runes[i:j]), " \t\r"))
			} else {
				for j+1 < to && !(runes[j] == '*' && runes[j+1] == '/') {
					j++
				}
				j = min(j+2, to)
				out = append(out, string(runes[i:j]))
			}
			i = j
		}
	}
	prev := 0
	for _, tok := range tokens {
		if tok.Type == TokenEOF {
			break
		}
		gap(prev, tok.Pos)
		prev = tok.EndPos
	}
	gap(prev, len(runes))
	return out
}

// allowedRolesValues reads the values of an `@allowedRoles(...)` spelling: the
// string literals between its parentheses. ok is false when anything else is
// written there.
func allowedRolesValues(text string) (values []string, ok bool) {
	tokens, err := NewLexer(text).Tokenize()
	if err != nil || len(tokens) < 4 || tokens[0].Type != TokenAt || tokens[2].Type != TokenParenOpen {
		return nil, false
	}
	for i := 3; i < len(tokens); i++ {
		switch tokens[i].Type {
		case TokenString:
			values = append(values, tokens[i].Literal)
		case TokenComma:
		case TokenParenClose:
			return values, len(values) > 0 && i+1 < len(tokens) && tokens[i+1].Type == TokenEOF
		default:
			return nil, false
		}
	}
	return nil, false
}

// End is the author's position just past the use: Line and Column advanced over
// Text, one column per rune, a newline starting the next line.
func (u DeprecatedUse) End() (line, column int) {
	line, column = u.Line, u.Column
	for _, r := range u.Text {
		if r == '\n' {
			line, column = line+1, 1
			continue
		}
		column++
	}
	return line, column
}

// ScanDeprecatedUses returns every use of a deprecated form src spells, in
// source order. It LEXES and does not parse, so it answers for a file whatever
// else is wrong with it, and strings and comments never reach it: the lexer
// hands back neither as tokens.
//
// Two forms are spelled today:
//
//   - `array(T)` (deprecation.ArrayType): the identifier `array` followed by a
//     `(` that closes, in a position the parser reads a type in -- after a
//     field's name (an identifier, or a keyword a field may be named after),
//     after the `]` of `[]T` or of a map's key, or as the element of an
//     enclosing `array(...)`. `array` is no function anywhere in the language,
//     so nothing else writes it before a `(`. Bare `array` is not the form.
//   - `@allowedRoles(...)` (deprecation.AllowedRoles): the annotation with an
//     argument list that closes, from its `@` to its `)`. An @allowedRoles
//     mentioned in a comment or a string -- a seed's @description names it --
//     is neither token, so it is not a use.
//
// A source the lexer refuses has no uses: nothing in it loads, and its lexer
// error is what refuses it.
func ScanDeprecatedUses(src string) []DeprecatedUse {
	scanned := scanDeprecatedUses(src)
	if len(scanned) == 0 {
		return nil
	}
	uses := make([]DeprecatedUse, len(scanned))
	for i, s := range scanned {
		uses[i] = s.use
	}
	return uses
}

// scannedUse is a use with the rune span it covers in the source, [start,
// end): what a rewrite replaces.
type scannedUse struct {
	use        DeprecatedUse
	start, end int
}

// scanDeprecatedUses is ScanDeprecatedUses keeping each use's span.
func scanDeprecatedUses(src string) []scannedUse {
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil
	}
	var (
		uses  []scannedUse
		runes []rune
	)
	span := func(from, to int) string {
		if runes == nil {
			runes = []rune(src)
		}
		return string(runes[tokens[from].Pos:tokens[to].EndPos])
	}
	for i := range tokens {
		if !arrayTypeAt(tokens, i) {
			continue
		}
		closeAt := matchingParenClose(tokens, i+1)
		if closeAt < 0 {
			continue
		}
		line, col := tokens[i].At()
		uses = append(uses, scannedUse{
			use:   DeprecatedUse{Rule: ruleDeprecatedArrayType, Line: line, Column: col, Text: span(i, closeAt)},
			start: tokens[i].Pos, end: tokens[closeAt].EndPos,
		})
	}
	for _, run := range annotationRuns(tokens) {
		for _, a := range run {
			if a.name != allowedRolesName || a.open < 0 || a.close < 0 {
				continue
			}
			var besides []string
			for _, other := range run {
				if other.at != a.at {
					besides = append(besides, other.name)
				}
			}
			sort.Strings(besides)
			line, col := tokens[a.at].At()
			uses = append(uses, scannedUse{
				use: DeprecatedUse{Rule: ruleDeprecatedAllowedRoles, Line: line, Column: col,
					Text: span(a.at, a.close), besides: strings.Join(besides, ",")},
				start: tokens[a.at].Pos, end: tokens[a.close].EndPos,
			})
		}
	}
	sort.SliceStable(uses, func(i, j int) bool { return uses[i].start < uses[j].start })
	return uses
}

// writtenAnnotation is one `@name` or `@name(...)` in a token stream: the
// index of its `@`, its name, and the indices of its parentheses (-1 when it
// has none, or they do not close).
type writtenAnnotation struct {
	at          int
	name        string
	open, close int
}

// annotationRuns groups a token stream's annotations into RUNS: annotations
// written one after another with nothing else between them, which is what a
// construct's leading annotations (or one field's trailing ones) are. An
// annotation name is an identifier or a keyword, as parseAttributeArgs reads
// one.
func annotationRuns(tokens []Token) [][]writtenAnnotation {
	var (
		runs [][]writtenAnnotation
		run  []writtenAnnotation
	)
	for i := 0; i < len(tokens); {
		if tokens[i].Type != TokenAt || i+1 >= len(tokens) ||
			(tokens[i+1].Type != TokenIdentifier && !isKeywordToken(tokens[i+1].Type)) {
			if len(run) > 0 {
				runs = append(runs, run)
				run = nil
			}
			i++
			continue
		}
		a := writtenAnnotation{at: i, name: tokens[i+1].Literal, open: -1, close: -1}
		next := i + 2
		if next < len(tokens) && tokens[next].Type == TokenParenOpen {
			a.open = next
			if closeAt := matchingParenClose(tokens, next); closeAt >= 0 {
				a.close = closeAt
				next = closeAt + 1
			} else {
				next = len(tokens)
			}
		}
		run = append(run, a)
		i = next
	}
	if len(run) > 0 {
		runs = append(runs, run)
	}
	return runs
}

// arrayTypeAt reports whether tokens[i] opens an `array(...)` in a type
// position (see ScanDeprecatedUses).
func arrayTypeAt(tokens []Token, i int) bool {
	if i == 0 || i+1 >= len(tokens) {
		return false
	}
	if tokens[i].Type != TokenIdentifier || tokens[i].Literal != "array" || tokens[i+1].Type != TokenParenOpen {
		return false
	}
	switch prev := tokens[i-1]; {
	case prev.Type == TokenIdentifier, isKeywordToken(prev.Type):
		return true // a field's name: `tags array(string)`
	case prev.Type == TokenBracketClose:
		return true // the element of `[]T`, or a map's value: `map[string]array(int)`
	case prev.Type == TokenParenOpen:
		// The element of an enclosing `array(...)`.
		return i >= 2 && tokens[i-2].Type == TokenIdentifier && tokens[i-2].Literal == "array"
	}
	return false
}

// matchingParenClose is the index of the `)` closing the `(` at tokens[open],
// or -1 when the source ends first.
func matchingParenClose(tokens []Token, open int) int {
	depth := 0
	for j := open; j < len(tokens); j++ {
		switch tokens[j].Type {
		case TokenParenOpen:
			depth++
		case TokenParenClose:
			depth--
			if depth == 0 {
				return j
			}
		case TokenEOF:
			return -1
		}
	}
	return -1
}

// DeprecatedUseRefusal is the refusal of a use of f once f's window is spent:
// the refusal the parser makes of the same spelling, word for word and
// positioned on the use. A load that finds the use reports it with this, so a
// load report and a parse say one thing about it.
func DeprecatedUseRefusal(u DeprecatedUse, f deprecation.Form) *RetiredFormError {
	endLine, endCol := u.End()
	return deprecatedFormRefusal(f, Token{Line: u.Line, Column: u.Column, EndLine: endLine, EndCol: endCol})
}

// deprecatedFormRefusal refuses f at tok.
func deprecatedFormRefusal(f deprecation.Form, tok Token) *RetiredFormError {
	return &RetiredFormError{
		Form:  RetiredForm{Spelling: f.Spelling, Replacement: f.Replacement, Rule: f.Rule},
		Parse: v1ParseErrorAt(tok, f.Refusal()),
	}
}

// refuseDeprecatedForm is the parser's half of the window, called with the
// current token on the `(` of a deprecated spelling whose first token is at:
// nil while the form registered under rule is still inside its window at the
// release this process decides at (deprecation.Current), and its refusal once
// the window is spent, spanning the spelling up to the `)` that closes it.
func (p *Parser) refuseDeprecatedForm(rule string, at Token) error {
	f, ok := deprecation.Lookup(rule)
	if !ok || !f.RefusesAt(deprecation.Current()) {
		return nil
	}
	refusal := deprecatedFormRefusal(f, at)
	if closeAt := matchingParenClose(p.tokens, p.pos); closeAt >= 0 {
		refusal.Parse.setEnd(p.tokens[closeAt])
	}
	return refusal
}
