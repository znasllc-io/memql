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
	"strings"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

// ruleDeprecatedArrayType is the rule parseTypeRef consults for `array(T)`.
const ruleDeprecatedArrayType = deprecation.ArrayType

// DeprecatedUse is one place a source spells a deprecated form.
type DeprecatedUse struct {
	// Rule is the form's rule (deprecation.Form.Rule).
	Rule string
	// Line and Column are where the spelling starts in the author's source,
	// 1-based; the column counts runes, as every Sense position does.
	Line, Column int
	// Text is the spelling as written, e.g. "array(string)".
	Text string
}

// Replacement is what to write over the use: the form's replacement with this
// use's own parts in it -- `array(string)` is `[]string`. ok is false for a use
// of a form this scanner does not spell, or one with nothing to carry across.
func (u DeprecatedUse) Replacement() (text string, ok bool) {
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
	}
	return "", false
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
// The one form today is `array(T)` (deprecation.ArrayType): the identifier
// `array` followed by a `(` that closes, in a position the parser reads a type
// in -- after a field's name (an identifier, or a keyword a field may be named
// after), after the `]` of `[]T` or of a map's key, or as the element of an
// enclosing `array(...)`. `array` is no function anywhere in the language, so
// nothing else writes it before a `(`. Bare `array` is not the form.
//
// A source the lexer refuses has no uses: nothing in it loads, and its lexer
// error is what refuses it.
func ScanDeprecatedUses(src string) []DeprecatedUse {
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil
	}
	var (
		uses  []DeprecatedUse
		runes []rune
	)
	for i := range tokens {
		if !arrayTypeAt(tokens, i) {
			continue
		}
		closeAt := matchingParenClose(tokens, i+1)
		if closeAt < 0 {
			continue
		}
		if runes == nil {
			runes = []rune(src)
		}
		line, col := tokens[i].At()
		uses = append(uses, DeprecatedUse{
			Rule:   ruleDeprecatedArrayType,
			Line:   line,
			Column: col,
			Text:   string(runes[tokens[i].Pos:tokens[closeAt].EndPos]),
		})
	}
	return uses
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
