package parser

import "strings"

// The #2766 codemod: fold `coalesce(a, b, ...)` calls into the `??`
// shorthand #2611 resurrected. `a ?? b ?? c` folds n-ary into ONE
// CoalesceExpr, so the rewrite is AST-identical to the call spelling --
// this is corpus hygiene, not a rejection: the call form keeps parsing.
//
// The rewrite is line-based and deliberately conservative, matching the
// documented memqlmigrate contract (leave input unchanged when the
// transformation cannot be performed safely). Two adjacencies are
// actively WRONG to rewrite, because #2611 bound `??` tighter than
// comparison but LOOSER than additive:
//
//	coalesce(a, 0) + 1   ->  a ?? 0 + 1    parses as a ?? (0 + 1)
//	!coalesce(a, false)  ->  !a ?? false   parses as (!a) ?? false
//
// and a top-level comparison or logical operator INSIDE an arg flips the
// same way (`coalesce(a == b, c)` -> `a == b ?? c` is `a == (b ?? c)`).
// Each of those leaves the call spelling alone. Comparison AFTER the
// call is the designed idiom and does rewrite: `a ?? "" == "active"`
// binds Swift-tight to `(a ?? "") == "active"`.
//
// A nested call collapses into the one flat chain, which is neutral
// under the #1614 selection rule (the final argument is the ultimate
// fallback, returned even when it resolves to ""; every non-final
// argument is skipped when nil or blank):
//
//	coalesce(a, coalesce(b, c))  ->  a, else (b non-blank ? b : c)
//	coalesce(a, b, c)            ->  a, else b non-blank ? b : c
//
// the same function of (a, b, c), because the inner call occupies the
// outer call's final slot.
//
// memql#2772 widened `??` to every value slot coalesce() reaches -- the
// `id:` row-identifier field, step-call named arguments, and brace /
// bracket literal arms all accept it now -- so the only skips left are
// the precedence hazards above plus what a LINE-based rewrite cannot
// see: a call spanning lines, and anything inside a comment.

// RewriteNullCoalesce rewrites same-line `coalesce(...)` calls carrying
// two or more arguments into the `??` chain. A call that spans lines, a
// call inside a comment or string, and the unsafe adjacencies above all
// pass through untouched.
func RewriteNullCoalesce(src []byte) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	inBlock := false
	changed := false
	for i, line := range lines {
		out, nextBlock, did := foldCoalesceLine(line, inBlock)
		inBlock = nextBlock
		if did {
			lines[i] = out
			changed = true
		}
	}
	if !changed {
		return src, nil
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// foldCoalesceLine folds every safe call on one line, rightmost-first so
// a nested call collapses inner-to-outer into a single flat chain.
//
// The carried block-comment state is computed from the ORIGINAL line:
// the rewrite only edits live-code regions, so it cannot change where a
// comment begins or ends.
func foldCoalesceLine(line string, inBlock bool) (string, bool, bool) {
	_, _, nextInBlock := scanLineRegions(line, inBlock)
	changed := false
	// Bounded: each pass removes one call, and a line holds far fewer
	// than this. The cap is a belt-and-braces stop, not a real limit.
	for pass := 0; pass < 64; pass++ {
		code, str, _ := scanLineRegions(line, inBlock)
		starts := coalesceCallStarts(line, code, str)
		progressed := false
		for k := len(starts) - 1; k >= 0; k-- {
			out, ok := foldCoalesceAt(line, starts[k], code, str)
			if !ok {
				continue
			}
			line = out
			changed = true
			progressed = true
			break
		}
		if !progressed {
			break
		}
	}
	return line, nextInBlock, changed
}

// scanLineRegions classifies each byte of the line: code reports whether
// the byte is live code (outside any comment), str whether it sits
// inside a string literal. String bytes are code too -- their content is
// load-bearing for argument splitting -- so the two masks overlap by
// design.
//
// The discriminating case (#2615/#2658): a `/*` inside a // comment or a
// string literal must NOT open a block span, or a real `*/` downstream
// phantom-blanks live corpus and the codemod silently skips real call
// sites.
func scanLineRegions(line string, inBlock bool) (code, str []bool, nextInBlock bool) {
	code = make([]bool, len(line))
	str = make([]bool, len(line))
	inStr := false
	for i := 0; i < len(line); i++ {
		switch {
		case inBlock:
			if line[i] == '*' && i+1 < len(line) && line[i+1] == '/' {
				i++ // consume the '/'; both bytes stay non-code
				inBlock = false
			}
		case inStr:
			code[i], str[i] = true, true
			if line[i] == '\\' && i+1 < len(line) {
				i++
				code[i], str[i] = true, true
				continue
			}
			if line[i] == '"' {
				inStr = false
			}
		case line[i] == '"':
			inStr = true
			code[i], str[i] = true, true
		case line[i] == '/' && i+1 < len(line) && line[i+1] == '/':
			// Line comment (including the /// doc form): the rest of
			// the line is prose. Crucially we stop here without ever
			// testing for /*, so a glob in the prose opens nothing.
			return code, str, inBlock
		case line[i] == '/' && i+1 < len(line) && line[i+1] == '*':
			inBlock = true
			i++
		default:
			code[i] = true
		}
	}
	return code, str, inBlock
}

const coalesceCall = "coalesce("

// coalesceCallStarts returns the offsets of live-code `coalesce(` calls,
// left to right. A preceding identifier byte or dot means a different
// symbol (`mycoalesce(`, `args.coalesce(`) and is not a call site.
func coalesceCallStarts(line string, code, str []bool) []int {
	var out []int
	for i := 0; i+len(coalesceCall) <= len(line); i++ {
		if line[i:i+len(coalesceCall)] != coalesceCall {
			continue
		}
		if !code[i] || str[i] {
			continue
		}
		if i > 0 && isIdentByte(line[i-1]) {
			continue
		}
		out = append(out, i)
	}
	return out
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '.' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// foldCoalesceAt folds the single call beginning at start, reporting
// false when the call cannot be rewritten safely.
func foldCoalesceAt(line string, start int, code, str []bool) (string, bool) {
	open := start + len(coalesceCall) - 1 // index of '('
	end, ok := matchCoalesceParen(line, open, code, str)
	if !ok {
		return "", false // spans lines, or runs into a comment
	}
	args, ok := splitCoalesceArgs(line, open+1, end, code, str)
	if !ok || len(args) < 2 {
		return "", false
	}
	for _, a := range args {
		if a == "" || hasTopLevelBoolOperator(a) {
			return "", false
		}
	}
	// `??` is looser than additive and looser than unary `!`, so an
	// arithmetic or negation neighbour would re-associate the fold.
	if b, found := prevCodeByte(line, start, code); found && strings.IndexByte("+-*/%!", b) >= 0 {
		return "", false
	}
	if b, found := nextCodeByte(line, end+1, code); found && strings.IndexByte("+-*/%.[(", b) >= 0 {
		// `.` / `[` / `(` are postfix accessors on the call RESULT.
		// Folding would re-attach them to the final ARM instead:
		// `coalesce(a, b).payload` -> `a ?? b.payload` (memql#2766 review).
		return "", false
	}
	return line[:start] + strings.Join(args, " ?? ") + line[end+1:], true
}

// matchCoalesceParen returns the index of the ')' closing the '(' at
// open. Bytes inside string literals never count, and a comment opening
// mid-call means the call does not close on this line.
func matchCoalesceParen(line string, open int, code, str []bool) (int, bool) {
	depth := 0
	for i := open; i < len(line); i++ {
		if str[i] {
			continue
		}
		if !code[i] {
			return 0, false
		}
		switch line[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// splitCoalesceArgs splits the argument list in line[lo:hi) on top-level
// commas, returning each argument trimmed of surrounding whitespace.
func splitCoalesceArgs(line string, lo, hi int, code, str []bool) ([]string, bool) {
	var args []string
	depth := 0
	prev := lo
	for i := lo; i < hi; i++ {
		if str[i] {
			continue
		}
		if !code[i] {
			return nil, false
		}
		switch line[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(line[prev:i]))
				prev = i + 1
			}
		}
	}
	return append(args, strings.TrimSpace(line[prev:hi])), true
}

// hasTopLevelBoolOperator reports whether the argument carries an
// unparenthesised comparison or logical operator. Such an argument
// re-associates under the fold, because `??` binds TIGHTER than both:
// `coalesce(a == b, c)` would become `a == b ?? c`, i.e. `a == (b ?? c)`.
// A `??` already present (an inner call folded on a previous pass) is
// fine -- that is the n-ary chain doing exactly what it should.
func hasTopLevelBoolOperator(arg string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '&', '|':
			if depth == 0 && i+1 < len(arg) && arg[i+1] == c {
				return true
			}
		case '<', '>':
			if depth == 0 {
				return true
			}
		case 'i':
			// `in` -- the DSL's single membership operator (CLAUDE.md).
			// It binds like a comparison, so an arm carrying one folds
			// wrong just as `==` does, and the result does not even
			// parse (memql#2766 review).
			if depth == 0 && isWordAt(arg, i, "in") {
				return true
			}
		case '=', '!':
			// `==` / `!=`. A lone `=` or `!` is not a comparison; `!`
			// leading a negated operand is safe under the fold.
			if depth == 0 && i+1 < len(arg) && arg[i+1] == '=' {
				return true
			}
		}
	}
	return false
}

// prevCodeByte returns the nearest live-code byte before idx, skipping
// horizontal whitespace.
func prevCodeByte(line string, idx int, code []bool) (byte, bool) {
	for i := idx - 1; i >= 0; i-- {
		if line[i] == ' ' || line[i] == '\t' {
			continue
		}
		if !code[i] {
			return 0, false
		}
		return line[i], true
	}
	return 0, false
}

// nextCodeByte returns the nearest live-code byte at or after idx,
// skipping horizontal whitespace.
func nextCodeByte(line string, idx int, code []bool) (byte, bool) {
	for i := idx; i < len(line); i++ {
		if line[i] == ' ' || line[i] == '\t' {
			continue
		}
		if !code[i] {
			return 0, false
		}
		return line[i], true
	}
	return 0, false
}

// isWordAt reports whether word sits at offset i as a whole word.
func isWordAt(s string, i int, word string) bool {
	if i+len(word) > len(s) || s[i:i+len(word)] != word {
		return false
	}
	if i > 0 && isIdentByte(s[i-1]) {
		return false
	}
	if end := i + len(word); end < len(s) && isIdentByte(s[end]) {
		return false
	}
	return true
}
