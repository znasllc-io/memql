package dslclause

// continuation.go owns two more answers every clause reader needs, for the
// same reason the keyword set above is owned here: a reader that re-derives
// them drifts, and the drift is silent (memql#2815).
//
//   - ContinuesClause: which physical lines belong to one clause. The
//     struct-query normaliser (component/language/parser
//     joinStructQueryContinuations) folds lines with it, and so must every
//     gate that reads a clause as text. A gate that stops at the first line
//     does not merely miss a line: it misses whatever the author moved onto
//     it, and edition 2026's codemod wraps every long filter at its top-level
//     `&&` (epic memql#5363) -- so a first-line reader of a wrapped filter
//     sees one conjunct of five.
//   - OpensLambda: whether a clause is written in the edition-2026 grammar.
//     Every predicate position there opens with a lambda header -- `filter
//     row => ...`, `spec b n = row => ...`, `@filter(row => ...)` -- which is
//     how a reader tells the two editions apart without parsing, and why the
//     codemod treats a clause that already opens with one as migrated.

import "strings"

// ContinuesClause reports whether the line next continues the clause text acc
// accumulated from the lines before it. Both are expected trimmed and free of
// comments.
//
// A line continues its predecessor when any of three things is true:
//
//   - acc leaves a `(` or `{` open -- a `when(...) {` guard or a parenthesised
//     group split across lines;
//   - acc ends on a dangling binary operator or opener, so it cannot be a
//     complete expression (`... &&`, `spec b n =`, `row =>`);
//   - next OPENS with a binary operator (`&& ...`), which is the other
//     conventional way to break a long boolean.
//
// Anything else starts a new clause. A clause keyword can therefore never be
// swallowed: `shape spaceFull` neither leaves a delimiter open nor ends on an
// operator, so the `sort` line after it starts fresh.
func ContinuesClause(acc, next string) bool {
	return unclosedDelimiters(acc) || endsOnDanglingOperator(acc) || opensWithBinaryOperator(next)
}

// ClauseExtent returns the index of the last line of the clause that opens on
// lines[i]: every later line ContinuesClause folds into it, with blank lines
// skipped exactly as the struct-query normaliser skips them. lines must be
// free of comments; string contents may be blanked or not.
//
// It adds one stop the normaliser never needs. A line opening with `}` while
// the clause has no delimiter of its own open closes the block the clause SITS
// IN -- the query's own brace. The normaliser only ever sees a construct's
// body, so it never meets that brace; a gate scanning a whole file does, and
// folding it in would hand the v1 parser `row => ... }`, which does not parse.
// A clause that does not parse guarantees nothing, so every owner-scoped query
// whose filter is its last clause would have read as unguarded.
func ClauseExtent(lines []string, i int) int {
	if i < 0 || i >= len(lines) {
		return i
	}
	acc := strings.TrimSpace(lines[i])
	last := i
	for j := i + 1; j < len(lines); j++ {
		next := strings.TrimSpace(lines[j])
		if next == "" {
			continue
		}
		if !ContinuesClause(acc, next) {
			break
		}
		if strings.HasPrefix(next, "}") && !unclosedDelimiters(acc) {
			break
		}
		acc += " " + next
		last = j
	}
	return last
}

// trailingOperators are the tokens that cannot END a complete expression,
// longest first so `<=` is tested before `<`. `>` also covers a lambda arrow
// `=>` left at the end of a line, and `?` a conditional broken after its
// condition.
var trailingOperators = []string{
	"??", "&&", "||", "==", "!=", "<=", ">=",
	"+", "-", "*", "/", "%", ",", "(", "{", "<", ">", "=", ".", ":", "?",
}

// leadingOperators are the tokens a continuation line may OPEN with. `-` is
// excluded deliberately: it is a legal identifier character in this language,
// so a line starting `-foo` is not reliably an operator.
//
// `?`, `:` and `=>` (memql#5364) are the edition-2026 continuations: a
// conditional broken before its branches, and a lambda whose arrow starts the
// next line (`filter row` / `=> row.a == 1`). No clause keyword starts with
// any of them, so none can swallow the next clause.
var leadingOperators = []string{"??", "&&", "||", "==", "!=", "<=", ">=", ")", "}", ",", ".", "+", "*", "/", "?", ":", "=>"}

func endsOnDanglingOperator(s string) bool {
	s = strings.TrimSpace(s)
	for _, op := range trailingOperators {
		if strings.HasSuffix(s, op) {
			return true
		}
	}
	return false
}

func opensWithBinaryOperator(s string) bool {
	s = strings.TrimSpace(s)
	for _, op := range leadingOperators {
		if strings.HasPrefix(s, op) {
			return true
		}
	}
	return false
}

// unclosedDelimiters reports whether s leaves a `(` or `{` open, ignoring
// anything inside a string literal so a `{` in `@pattern("^a{2}$")` does not
// read as an opener.
func unclosedDelimiters(s string) bool {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '{':
			depth++
		case ')', '}':
			depth--
		}
	}
	return depth > 0
}

// OpensLambda reports whether clause opens with an edition-2026 lambda header
// -- `x =>`, `() =>`, `(x) =>`, `(x, y) =>`, a trailing comma allowed in the
// list -- after optional leading whitespace. The arrow may sit on the next
// line: `=>` opening a line is a continuation (leadingOperators).
//
// It is a TEXT test on purpose. It decides which grammar a reader should use
// before any parse happens, and the readers that need it include ones that
// must not import the parser (Sense, which the editor and the conformance
// suite both consume). A clause it accepts is then parsed with the v1 grammar
// by whoever can; one it rejects is read as the legacy grammar it still is.
// It is also the rule the struct-query rewriter asks of a `refine` clause, so
// the loader and the gates agree about what a lambda is.
func OpensLambda(clause string) bool {
	_, _, ok := lambdaHeader(clause)
	return ok
}

// SplitLambdaHeader returns the parameter names and the body text of a clause
// that OpensLambda accepts. ok is false for any other clause.
func SplitLambdaHeader(clause string) (params []string, body string, ok bool) {
	return lambdaHeader(clause)
}

func lambdaHeader(clause string) (params []string, body string, ok bool) {
	s := strings.TrimLeft(clause, " \t\r\n")
	if strings.HasPrefix(s, "(") {
		close := strings.IndexByte(s, ')')
		if close < 0 {
			return nil, "", false
		}
		list := strings.TrimSpace(s[1:close])
		if list != "" {
			parts := strings.Split(list, ",")
			if strings.TrimSpace(parts[len(parts)-1]) == "" && len(parts) > 1 {
				parts = parts[:len(parts)-1] // `(x,) =>`
			}
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if !isIdent(p) {
					return nil, "", false
				}
				params = append(params, p)
			}
		}
		s = s[close+1:]
	} else {
		end := 0
		for end < len(s) && isIdentByte(s[end], end == 0) {
			end++
		}
		if end == 0 {
			return nil, "", false
		}
		params = []string{s[:end]}
		s = s[end:]
	}
	s = strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(s, "=>") {
		return nil, "", false
	}
	return params, strings.TrimSpace(s[2:]), true
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i], i == 0) {
			return false
		}
	}
	return true
}

func isIdentByte(c byte, first bool) bool {
	switch {
	case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
}
