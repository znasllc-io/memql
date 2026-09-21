// Package bnfmatch reads the grammar MemQL publishes and recognizes a MemQL
// token stream against it.
//
// # Why it reads the PUBLISHED text
//
// dslspec.BNF() renders the authoring grammar, cmd/dslgrammar commits it as
// docs/public/language/grammar.md, and the memqlGrammar() builtin serves it.
// Those bytes are the artifact: a model is given them in a prompt, or a
// decoder is constrained by them. Every other grammar test in this tree is a
// NAME-level agreement check -- that a construct has a production, that the
// operator ladder names the table's levels -- so a structurally wrong
// right-hand side (a missing optional, a wrong repetition, two clauses nested
// the wrong way round) ships green and teaches a model a form the parser
// refuses.
//
// So this package interprets the published notation itself rather than a Go
// structure the page happens to render from. A second structure would need its
// own proof that the text and the structure agree, and that proof is the part
// that rots. It also means no external EBNF library: the notation below is
// MemQL's own, and a dependency added to dslspec's module would reach every
// module that imports it (see the package's non-test import set).
//
// # The notation
//
// The grammar states it in its own header, and this is the whole of it:
//
//	<name> is a production, "x" a terminal, [ x ] optional,
//	{ x } zero or more, x* zero or more, ( a | b ) a choice.
//
// Plus three shapes the emitted text uses that the header does not spell:
// 'x' is a terminal whose text contains a double quote, x+ is one or more,
// and "a".."z" is a character range (reachable only from a lexical
// production). A right-hand side that is prose rather than notation --
// <character> is one -- parses as Prose and matches nothing; reaching one is
// reported rather than silently accepted.
package bnfmatch

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/parser"
)

// Expr is one node of a production's right-hand side.
type Expr interface{ exprNode() }

// Ref names another production: `<name>`.
type Ref struct{ Name string }

// Term is a quoted terminal. Text is what the grammar wrote; Tokens is what
// the MemQL lexer makes of it, because a terminal is matched against the token
// stream and several of them are more than one token (`[]` is two, and
// `@description` is `@` then an identifier). LexErr records a terminal the
// lexer refuses, and a Term with no tokens matches nothing -- `///` is one,
// since the lexer takes doc comments off on a side channel.
type Term struct {
	Text   string
	Tokens []parser.Token
	LexErr error
}

// Range is `"a".."z"`, a character range. It belongs to a lexical production
// and matches one rune, never a token.
type Range struct{ Lo, Hi rune }

// Seq is juxtaposition: every part in order.
type Seq []Expr

// Alt is `a | b`: any one part.
type Alt []Expr

// Opt is `[ x ]`.
type Opt struct{ Body Expr }

// Rep is `{ x }` and `x*`: zero or more.
type Rep struct{ Body Expr }

// Plus is `x+`: one or more.
type Plus struct{ Body Expr }

// Prose is a right-hand side written as English rather than notation. It
// matches nothing; a recognition that reaches one records the name, because a
// production the recognizer cannot read is a hole in the proof, not a pass.
type Prose struct{ Text string }

func (Ref) exprNode()   {}
func (Term) exprNode()  {}
func (Range) exprNode() {}
func (Seq) exprNode()   {}
func (Alt) exprNode()   {}
func (Opt) exprNode()   {}
func (Rep) exprNode()   {}
func (Plus) exprNode()  {}
func (Prose) exprNode() {}

// Production is one `<name> ::= rhs` of the published grammar.
type Production struct {
	Name string
	Expr Expr
	// RHS is the right-hand side as the page writes it, for failure messages:
	// a reader given the production text can see what the recognizer saw.
	RHS string
}

// Grammar is every production of one published grammar, by name, in the order
// the page lists them.
type Grammar struct {
	Prods map[string]*Production
	Order []string
}

// Production returns the named production, or nil.
func (g *Grammar) Production(name string) *Production {
	if g == nil {
		return nil
	}
	return g.Prods[name]
}

// Undefined names every production some right-hand side references that the
// grammar does not define. A published grammar with one cannot derive the
// language: the reference is a dead end wherever it is reached.
func (g *Grammar) Undefined() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range g.Order {
		walk(g.Prods[name].Expr, func(e Expr) {
			ref, ok := e.(Ref)
			if !ok || g.Prods[ref.Name] != nil || seen[ref.Name] {
				return
			}
			seen[ref.Name] = true
			out = append(out, ref.Name)
		})
	}
	return out
}

// InvisibleTerminals names every terminal the grammar writes that the MemQL
// lexer reads cleanly as NO TOKEN at all, in the order the productions list
// them. Such a terminal cannot be matched at token level, so a caller pins the
// set it expects rather than discovering a new one as an unexplained refusal.
func (g *Grammar) InvisibleTerminals() []string {
	return g.terminalsWhere(func(t Term) bool { return t.LexErr == nil && len(t.Tokens) == 0 })
}

// CharacterTerminals names every terminal the MemQL lexer REFUSES -- a bare
// quote is one. These belong to a lexical production, where they are matched
// against the characters of a token; reaching one at token level is a defect,
// and Result.CharacterTerminals reports that.
func (g *Grammar) CharacterTerminals() []string {
	return g.terminalsWhere(func(t Term) bool { return t.LexErr != nil })
}

func (g *Grammar) terminalsWhere(keep func(Term) bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range g.Order {
		walk(g.Prods[name].Expr, func(e Expr) {
			t, ok := e.(Term)
			if !ok || !keep(t) || seen[t.Text] {
				return
			}
			seen[t.Text] = true
			out = append(out, t.Text)
		})
	}
	return out
}

// walk visits every node of an expression tree.
func walk(e Expr, fn func(Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch x := e.(type) {
	case Seq:
		for _, part := range x {
			walk(part, fn)
		}
	case Alt:
		for _, part := range x {
			walk(part, fn)
		}
	case Opt:
		walk(x.Body, fn)
	case Rep:
		walk(x.Body, fn)
	case Plus:
		walk(x.Body, fn)
	}
}

// ParseGrammar reads the published grammar text into productions. The text is
// what dslspec.BNF() renders and what the fenced block of
// docs/public/language/grammar.md holds: `(* ... *)` comments, blank lines,
// and `<name> ::= rhs` productions whose alternation may continue on lines
// opening with a bar.
func ParseGrammar(bnf string) (*Grammar, error) {
	g := &Grammar{Prods: map[string]*Production{}}
	current := ""
	inComment := false
	for n, line := range strings.Split(bnf, "\n") {
		trimmed := strings.TrimSpace(line)
		// A comment runs to its closing marker; the ladder's header spans two
		// lines, and the second one must not be read as a continuation.
		if inComment {
			if strings.Contains(trimmed, "*)") {
				inComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "(*") {
			inComment = !strings.Contains(trimmed, "*)")
			current = ""
			continue
		}
		if trimmed == "" {
			current = ""
			continue
		}
		name, rhs, isHead := splitProduction(trimmed)
		switch {
		case isHead:
			if g.Prods[name] != nil {
				return nil, fmt.Errorf("line %d: <%s> is declared twice; a production declared twice has "+
					"two right-hand sides and the grammar cannot say which one it means", n+1, name)
			}
			current = name
			p := &Production{Name: name, RHS: rhs}
			g.Prods[name] = p
			g.Order = append(g.Order, name)
		case current != "" && strings.HasPrefix(trimmed, "|"):
			g.Prods[current].RHS += " " + trimmed
		default:
			return nil, fmt.Errorf("line %d: %q is neither a production nor a continuation of one; "+
				"the recognizer reads the page as the grammar, so a line it cannot place is a line it "+
				"would silently ignore", n+1, trimmed)
		}
	}
	if len(g.Order) == 0 {
		return nil, fmt.Errorf("the grammar text holds no productions at all; either BNF() stopped " +
			"emitting them or the `<name> ::= rhs` shape changed under this reader")
	}
	for _, name := range g.Order {
		p := g.Prods[name]
		expr, err := parseRHS(p.RHS)
		if err != nil {
			return nil, fmt.Errorf("<%s> ::= %s: %w", name, p.RHS, err)
		}
		p.Expr = expr
	}
	return g, nil
}

// splitProduction reads a `<name> ::= rhs` head.
func splitProduction(line string) (name, rhs string, ok bool) {
	if !strings.HasPrefix(line, "<") {
		return "", "", false
	}
	close := strings.Index(line, ">")
	if close < 0 {
		return "", "", false
	}
	rest := strings.TrimSpace(line[close+1:])
	if !strings.HasPrefix(rest, "::=") {
		return "", "", false
	}
	return line[1:close], strings.TrimSpace(rest[3:]), true
}

// ---- the right-hand side ----

type ntokKind int

const (
	ntRef ntokKind = iota
	ntTerm
	ntOpen   // ( [ {
	ntClose  // ) ] }
	ntBar    // |
	ntStar   // *
	ntPlus   // +
	ntDotDot // ..
	ntProse  // anything the notation does not spell
)

type ntok struct {
	kind ntokKind
	text string
	// open is the bracket a ntOpen/ntClose carries, so a group's closer can be
	// held to its opener.
	open byte
}

// lexRHS reads a right-hand side into notation tokens. Anything the notation
// does not spell ends the scan as prose: the production is then English, and
// the recognizer says so rather than deriving something from half of it.
func lexRHS(s string) []ntok {
	var out []ntok
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return append(out, ntok{kind: ntProse, text: s[i:]})
			}
			out = append(out, ntok{kind: ntRef, text: s[i+1 : i+end]})
			i += end + 1
		case c == '"' || c == '\'':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return append(out, ntok{kind: ntProse, text: s[i:]})
			}
			out = append(out, ntok{kind: ntTerm, text: s[i+1 : i+1+end]})
			i += end + 2
		case c == '(' || c == '[' || c == '{':
			out = append(out, ntok{kind: ntOpen, open: c})
			i++
		case c == ')' || c == ']' || c == '}':
			out = append(out, ntok{kind: ntClose, open: c})
			i++
		case c == '|':
			out = append(out, ntok{kind: ntBar})
			i++
		case c == '*':
			out = append(out, ntok{kind: ntStar})
			i++
		case c == '+':
			out = append(out, ntok{kind: ntPlus})
			i++
		case c == '.' && i+1 < len(s) && s[i+1] == '.':
			out = append(out, ntok{kind: ntDotDot})
			i += 2
		default:
			return append(out, ntok{kind: ntProse, text: strings.TrimSpace(s[i:])})
		}
	}
	return out
}

// parseRHS reads one right-hand side. The grammar of the notation is
// alternation over sequences over postfixed primaries, which is the whole
// language the page writes.
func parseRHS(rhs string) (Expr, error) {
	toks := lexRHS(rhs)
	for _, t := range toks {
		if t.kind == ntProse {
			return Prose{Text: rhs}, nil
		}
	}
	p := &rhsParser{toks: toks}
	e, err := p.alt()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unread notation from token %d", p.pos)
	}
	return e, nil
}

type rhsParser struct {
	toks []ntok
	pos  int
}

func (p *rhsParser) peek() (ntok, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return ntok{}, false
}

func (p *rhsParser) alt() (Expr, error) {
	var parts []Expr
	for {
		e, err := p.seq()
		if err != nil {
			return nil, err
		}
		parts = append(parts, e)
		t, ok := p.peek()
		if !ok || t.kind != ntBar {
			break
		}
		p.pos++
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return Alt(parts), nil
}

func (p *rhsParser) seq() (Expr, error) {
	var parts []Expr
	for {
		t, ok := p.peek()
		if !ok || t.kind == ntBar || t.kind == ntClose {
			break
		}
		e, err := p.postfix()
		if err != nil {
			return nil, err
		}
		parts = append(parts, e)
	}
	switch len(parts) {
	case 0:
		// An empty alternative -- `a | | b` -- is not a shape this grammar
		// writes, and silently reading one as "matches nothing" would make
		// every alternation it appeared in trivially satisfiable.
		return nil, fmt.Errorf("an alternative with nothing in it at token %d", p.pos)
	case 1:
		return parts[0], nil
	default:
		return Seq(parts), nil
	}
}

func (p *rhsParser) postfix() (Expr, error) {
	e, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		t, ok := p.peek()
		if !ok {
			return e, nil
		}
		switch t.kind {
		case ntStar:
			p.pos++
			e = Rep{Body: e}
		case ntPlus:
			p.pos++
			e = Plus{Body: e}
		default:
			return e, nil
		}
	}
}

func (p *rhsParser) primary() (Expr, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("the notation ends where a production, a terminal or a group was due")
	}
	switch t.kind {
	case ntRef:
		p.pos++
		return Ref{Name: t.text}, nil
	case ntTerm:
		p.pos++
		if next, ok := p.peek(); ok && next.kind == ntDotDot {
			p.pos++
			hi, ok := p.peek()
			if !ok || hi.kind != ntTerm {
				return nil, fmt.Errorf("%q .. is not followed by a terminal", t.text)
			}
			p.pos++
			lo, _ := utf8.DecodeRuneInString(t.text)
			hiR, _ := utf8.DecodeRuneInString(hi.text)
			return Range{Lo: lo, Hi: hiR}, nil
		}
		return newTerm(t.text), nil
	case ntOpen:
		open := t.open
		p.pos++
		body, err := p.alt()
		if err != nil {
			return nil, err
		}
		closer, ok := p.peek()
		if !ok || closer.kind != ntClose || matching(open) != closer.open {
			return nil, fmt.Errorf("a group opened with %q is not closed with %q", string(open), string(matching(open)))
		}
		p.pos++
		switch open {
		case '(':
			return body, nil
		case '[':
			return Opt{Body: body}, nil
		default:
			return Rep{Body: body}, nil
		}
	}
	return nil, fmt.Errorf("unexpected notation at token %d", p.pos)
}

func matching(open byte) byte {
	switch open {
	case '(':
		return ')'
	case '[':
		return ']'
	}
	return '}'
}

// newTerm lexes a terminal once, so matching it against the input is a
// comparison of token streams rather than of text. A terminal the lexer reads
// as more than one token (`[]`, `@description`) matches that many tokens, and
// one it reads as none (`///`) matches nothing -- which is the honest answer at
// token level, because the lexer takes doc comments off on a side channel and
// the parser never sees them.
func newTerm(text string) Term {
	t := Term{Text: text}
	toks, err := parser.NewLexer(text).Tokenize()
	if err != nil {
		t.LexErr = err
		return t
	}
	for _, tok := range toks {
		if tok.Type == parser.TokenEOF {
			continue
		}
		t.Tokens = append(t.Tokens, tok)
	}
	return t
}
