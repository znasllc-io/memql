package bnfmatch

// The recognizer.
//
// # Token level, not character level
//
// The input is the MemQL lexer's own token stream, so no lexical detail --
// whitespace, a comment, a string escape, the column a clause starts on -- can
// make a structurally correct file fail. That is also what makes a failure
// mean something: the recognizer stopped because no production derives the
// tokens, not because the text was laid out unusually.
//
// Three facts about that stream the grammar does not describe, and how each is
// reconciled here rather than by loosening the grammar:
//
//   - The lexer FUSES a dotted path into one identifier token (`row.createdAt`
//     is one token; `.count` after a `]` is one token). The grammar spells
//     member access as "." <name>, so a fused token is read as its segments
//     and the dots between them, and Furthest is always an index into the
//     tokens the caller passed.
//   - A `///` doc comment produces NO token at all: the lexer takes it off on
//     a side channel. <doc-comment> is therefore unreachable at token level,
//     which is why every use of it in the grammar is under a `*`.
//   - <name>, <number> and <string> are LEXICAL productions. A <string> is
//     matched by the token's kind, because a string token carries its decoded
//     value rather than its spelling and the production is written over the
//     spelling. A <name> and a <number> are matched by kind AND by holding the
//     token's text to the production character by character, so the recognizer
//     is never looser than the grammar it is proving.
//
// # Algorithm
//
// Memoized end-position sets: ends(e, p) is every position at which a
// derivation of e starting at token p can end. A production's set is memoized
// per start position, so this is a packrat recognizer that keeps every
// alternative -- ambiguity costs a bigger set, never a wrong answer. The input
// derives when len(tokens) is in ends(start, 0).
//
// Left recursion is a grammar defect rather than something to support: a
// production re-entered at the position it is already being computed at
// contributes no ends there, and its name is reported.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The lexical productions. A reference to one of these consumes exactly one
// token, decided by the token's kind and then by the production's own
// characters.
const (
	ClassName   = "name"
	ClassNumber = "number"
	ClassString = "string"
)

// charOnly names the lexical productions that describe CHARACTERS of a token
// rather than tokens. Reaching one at token level is a grammar defect -- there
// is no token a `<digit>` is -- so it matches nothing and is reported.
var charOnly = map[string]bool{"character": true, "digit": true, "text": true}

// Result is one recognition's full answer.
type Result struct {
	// OK reports whether the tokens derive from the start production.
	OK bool
	// Furthest is the index, into the tokens the CALLER passed, of the
	// furthest token the recognizer tried to match; len(tokens) when it ran
	// out of input. When OK is false this is where the input stopped making
	// sense.
	Furthest int
	// Stack is the production names in scope when Furthest was last raised,
	// outermost first. Best effort: a production already memoized at that
	// position is not re-entered, so the stack names where the recognizer
	// reached that token, not every path that could have.
	Stack []string
	// Undefined names each production referenced during the recognition that
	// the grammar does not define.
	Undefined []string
	// Prose names each production reached whose right-hand side is English
	// rather than notation.
	Prose []string
	// CharOnly names each character-level production reached at token level.
	CharOnly []string
	// InvisibleTerminals names each terminal reached that the lexer reads as
	// NO TOKEN AT ALL, so nothing at token level can match it. `///` is one
	// and is not a defect: the lexer takes doc comments off on a side channel
	// before the parser sees them, which is exactly why every use of
	// <doc-comment> in the grammar sits under a `*`. A caller pins the set it
	// expects rather than ignoring it, so a NEW one is a finding.
	InvisibleTerminals []string
	// CharacterTerminals names each terminal reached at token level that the
	// lexer REFUSES -- a bare quote is one. It belongs to a lexical
	// production, matched against a token's characters; reaching it at token
	// level is the same defect as reaching <digit> there.
	CharacterTerminals []string
	// LeftRecursive names each production found re-entered at the position it
	// was being computed at.
	LeftRecursive []string
}

// Where renders the failure position against the tokens the caller passed:
// the line and column of the token the recognizer stopped on, and the token
// itself.
func (r Result) Where(tokens []parser.Token) string {
	if r.Furthest >= len(tokens) {
		return "the end of the input"
	}
	t := tokens[r.Furthest]
	line, col := t.At()
	return fmt.Sprintf("line %d, column %d (token %q)", line, col, t.Literal)
}

// In renders the productions in scope where the recognition stopped,
// innermost first, which is the production the input failed inside.
func (r Result) In() string {
	if len(r.Stack) == 0 {
		return "<unknown>"
	}
	parts := make([]string, 0, len(r.Stack))
	for i := len(r.Stack) - 1; i >= 0; i-- {
		parts = append(parts, "<"+r.Stack[i]+">")
	}
	return strings.Join(parts, " <- ")
}

// Tokens lexes MemQL source into the stream the recognizer reads, EOF
// excluded. It is the AUTHORED text that is lexed: the grammar describes the
// authoring surface, not the internal form the struct-form rewriter lowers a
// query or a mutation into.
func Tokens(src string) ([]parser.Token, error) {
	toks, err := parser.NewLexer(src).Tokenize()
	if err != nil {
		return nil, err
	}
	out := make([]parser.Token, 0, len(toks))
	for _, t := range toks {
		if t.Type == parser.TokenEOF {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// Match recognizes tokens against the named start production of g.
func Match(g *Grammar, start string, tokens []parser.Token) Result {
	split, origin := splitFusedPaths(tokens)
	m := &matcher{
		g:          g,
		toks:       split,
		memo:       map[memoKey][]int{},
		inProgress: map[memoKey]bool{},
		reported:   map[string]bool{},
	}
	var ends []int
	if prod := g.Production(start); prod != nil {
		ends = m.production(prod, 0)
	} else {
		m.note(&m.res.Undefined, start)
	}
	m.res.OK = contains(ends, len(split))
	if m.furthest >= len(split) {
		m.res.Furthest = len(tokens)
	} else {
		m.res.Furthest = origin[m.furthest]
	}
	m.res.Stack = m.furthestStack
	return m.res
}

type memoKey struct {
	name string
	pos  int
}

type matcher struct {
	g          *Grammar
	toks       []parser.Token
	memo       map[memoKey][]int
	inProgress map[memoKey]bool
	// spelled caches, per lexical class, whether a token's text is spelled by
	// the class's own production.
	spelled       map[string]map[string]bool
	stack         []string
	furthest      int
	furthestStack []string
	reported      map[string]bool
	res           Result
}

// note records a grammar defect once, in the order they were met.
func (m *matcher) note(into *[]string, name string) {
	key := name + "\x00"
	if m.reported[key] {
		return
	}
	m.reported[key] = true
	*into = append(*into, name)
}

// production returns the memoized end set of prod started at pos.
func (m *matcher) production(prod *Production, pos int) []int {
	key := memoKey{prod.Name, pos}
	if ends, done := m.memo[key]; done {
		return ends
	}
	if m.inProgress[key] {
		m.note(&m.res.LeftRecursive, prod.Name)
		return nil
	}
	m.inProgress[key] = true
	m.stack = append(m.stack, prod.Name)
	ends := m.expr(prod.Expr, pos)
	m.stack = m.stack[:len(m.stack)-1]
	delete(m.inProgress, key)
	m.memo[key] = ends
	return ends
}

// expr returns the sorted, duplicate-free end set of e started at token pos.
func (m *matcher) expr(e Expr, pos int) []int {
	switch x := e.(type) {
	case nil:
		return []int{pos}
	case Ref:
		return m.ref(x.Name, pos)
	case Term:
		return m.term(x, pos)
	case Range:
		// A range belongs to a lexical production and matches a character,
		// never a token. Reaching one here is the same defect as reaching
		// <digit>.
		m.note(&m.res.CharOnly, "a character range")
		return nil
	case Prose:
		return nil
	case Seq:
		cur := []int{pos}
		for _, part := range x {
			var more []int
			for _, p := range cur {
				more = union(more, m.expr(part, p))
			}
			if len(more) == 0 {
				return nil
			}
			cur = more
		}
		return cur
	case Alt:
		var out []int
		for _, part := range x {
			out = union(out, m.expr(part, pos))
		}
		return out
	case Opt:
		return union([]int{pos}, m.expr(x.Body, pos))
	case Rep:
		return m.closure(x.Body, pos, true)
	case Plus:
		return m.closure(x.Body, pos, false)
	}
	return nil
}

// closure is the fixpoint of repeating body from pos. withEmpty includes pos
// itself, which is the difference between `{ x }` and `x+`.
func (m *matcher) closure(body Expr, pos int, withEmpty bool) []int {
	seen := map[int]bool{}
	if withEmpty {
		seen[pos] = true
	}
	queue := []int{pos}
	visited := map[int]bool{pos: true}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, end := range m.expr(body, p) {
			seen[end] = true
			if !visited[end] {
				visited[end] = true
				queue = append(queue, end)
			}
		}
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// ref resolves a production reference at token level.
func (m *matcher) ref(name string, pos int) []int {
	switch {
	case name == ClassString:
		return m.class(name, pos)
	case name == ClassName || name == ClassNumber:
		return m.class(name, pos)
	case charOnly[name]:
		m.note(&m.res.CharOnly, name)
		return nil
	}
	prod := m.g.Production(name)
	if prod == nil {
		m.note(&m.res.Undefined, name)
		return nil
	}
	if _, isProse := prod.Expr.(Prose); isProse {
		m.note(&m.res.Prose, name)
		return nil
	}
	return m.production(prod, pos)
}

// class matches one token of a lexical class.
func (m *matcher) class(name string, pos int) []int {
	if !m.at(pos) {
		return nil
	}
	tok := m.toks[pos]
	switch name {
	case ClassString:
		if tok.Type != parser.TokenString {
			return nil
		}
		// A string token carries its DECODED value, and the production is
		// written over the spelling (a quote, characters, a quote), so the
		// kind is the whole test.
		return []int{pos + 1}
	case ClassNumber:
		if tok.Type != parser.TokenNumber {
			return nil
		}
	case ClassName:
		// Every token but a string or a number may be a name: an identifier,
		// and a keyword token, whose text is a name the lexer promoted (a
		// construct may be called `default`). The production below decides.
		if tok.Type == parser.TokenString || tok.Type == parser.TokenNumber {
			return nil
		}
	}
	if !m.spells(name, tok.Literal) {
		return nil
	}
	return []int{pos + 1}
}

// spells reports whether the lexical production of class spells text whole.
// A class the grammar gives no production is matched by kind alone, and the
// omission is reported.
func (m *matcher) spells(class, text string) bool {
	prod := m.g.Production(class)
	if prod == nil {
		m.note(&m.res.Undefined, class)
		return true
	}
	if m.spelled == nil {
		m.spelled = map[string]map[string]bool{}
	}
	cache := m.spelled[class]
	if cache == nil {
		cache = map[string]bool{}
		m.spelled[class] = cache
	}
	if ok, done := cache[text]; done {
		return ok
	}
	c := &charMatcher{g: m.g, runes: []rune(text), memo: map[memoKey][]int{}, inProgress: map[memoKey]bool{}}
	ok := contains(c.production(prod, 0), len(c.runes))
	cache[text] = ok
	return ok
}

// term matches a quoted terminal: the token sequence the lexer reads it as. A
// string token never matches a terminal, so the terminal "query" is not the
// string "query".
func (m *matcher) term(t Term, pos int) []int {
	if len(t.Tokens) == 0 {
		if t.LexErr != nil {
			m.note(&m.res.CharacterTerminals, t.Text)
		} else {
			m.note(&m.res.InvisibleTerminals, t.Text)
		}
		m.at(pos)
		return nil
	}
	for i, want := range t.Tokens {
		if !m.at(pos + i) {
			return nil
		}
		got := m.toks[pos+i]
		if got.Type == parser.TokenString || got.Literal != want.Literal {
			return nil
		}
	}
	return []int{pos + len(t.Tokens)}
}

// at reports whether pos is a token, recording it as the furthest one tried
// along with the productions in scope.
func (m *matcher) at(pos int) bool {
	if pos > m.furthest {
		m.furthest = pos
		m.furthestStack = append([]string(nil), m.stack...)
	}
	return pos < len(m.toks)
}

// ---- the character level ----

// charMatcher reads a lexical production over the characters of one token,
// with the same end-set algorithm the token level uses.
type charMatcher struct {
	g          *Grammar
	runes      []rune
	memo       map[memoKey][]int
	inProgress map[memoKey]bool
}

func (c *charMatcher) production(prod *Production, pos int) []int {
	key := memoKey{prod.Name, pos}
	if ends, done := c.memo[key]; done {
		return ends
	}
	if c.inProgress[key] {
		return nil
	}
	c.inProgress[key] = true
	ends := c.expr(prod.Expr, pos)
	delete(c.inProgress, key)
	c.memo[key] = ends
	return ends
}

func (c *charMatcher) expr(e Expr, pos int) []int {
	switch x := e.(type) {
	case nil:
		return []int{pos}
	case Ref:
		prod := c.g.Production(x.Name)
		if prod == nil {
			return nil
		}
		return c.production(prod, pos)
	case Term:
		want := []rune(x.Text)
		if pos+len(want) > len(c.runes) {
			return nil
		}
		for i, r := range want {
			if c.runes[pos+i] != r {
				return nil
			}
		}
		return []int{pos + len(want)}
	case Range:
		if pos < len(c.runes) && c.runes[pos] >= x.Lo && c.runes[pos] <= x.Hi {
			return []int{pos + 1}
		}
		return nil
	case Prose:
		return nil
	case Seq:
		cur := []int{pos}
		for _, part := range x {
			var more []int
			for _, p := range cur {
				more = union(more, c.expr(part, p))
			}
			if len(more) == 0 {
				return nil
			}
			cur = more
		}
		return cur
	case Alt:
		var out []int
		for _, part := range x {
			out = union(out, c.expr(part, pos))
		}
		return out
	case Opt:
		return union([]int{pos}, c.expr(x.Body, pos))
	case Rep:
		return c.closure(x.Body, pos, true)
	case Plus:
		return c.closure(x.Body, pos, false)
	}
	return nil
}

func (c *charMatcher) closure(body Expr, pos int, withEmpty bool) []int {
	seen := map[int]bool{}
	if withEmpty {
		seen[pos] = true
	}
	queue := []int{pos}
	visited := map[int]bool{pos: true}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, end := range c.expr(body, p) {
			seen[end] = true
			if !visited[end] {
				visited[end] = true
				queue = append(queue, end)
			}
		}
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// ---- the token stream ----

// splitFusedPaths reads each fused dotted identifier as its segments and the
// dots between them, returning the split stream and, for each split token, the
// index of the token it came from. A colon-bearing identifier (`v1:todos:todo`
// unquoted) is left whole on purpose: the grammar has no production for one,
// and splitting it here would make the recognizer looser than the grammar it
// is proving.
func splitFusedPaths(tokens []parser.Token) ([]parser.Token, []int) {
	out := make([]parser.Token, 0, len(tokens))
	origin := make([]int, 0, len(tokens))
	for i, tok := range tokens {
		if tok.Type != parser.TokenIdentifier || !strings.Contains(tok.Literal, ".") {
			out = append(out, tok)
			origin = append(origin, i)
			continue
		}
		lit := tok.Literal
		if strings.HasPrefix(lit, ".") {
			out = append(out, parser.Token{Type: parser.TokenDot, Literal: ".", Line: tok.Line, Column: tok.Column,
				AuthoredLine: tok.AuthoredLine, AuthoredCol: tok.AuthoredCol})
			origin = append(origin, i)
			lit = lit[1:]
		}
		for j, seg := range strings.Split(lit, ".") {
			if j > 0 {
				out = append(out, parser.Token{Type: parser.TokenDot, Literal: ".", Line: tok.Line, Column: tok.Column,
					AuthoredLine: tok.AuthoredLine, AuthoredCol: tok.AuthoredCol})
				origin = append(origin, i)
			}
			typ := parser.TokenIdentifier
			if seg != "" && isDigits(seg) {
				typ = parser.TokenNumber
			}
			out = append(out, parser.Token{Type: typ, Literal: seg, Line: tok.Line, Column: tok.Column,
				AuthoredLine: tok.AuthoredLine, AuthoredCol: tok.AuthoredCol})
			origin = append(origin, i)
		}
	}
	return out, origin
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// union merges two sorted, duplicate-free sets.
func union(a, b []int) []int {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

func contains(set []int, v int) bool {
	i := sort.SearchInts(set, v)
	return i < len(set) && set[i] == v
}
