package baseparser

import "strings"

// comment_blank.go provides an offset-preserving comment scrubber used
// by the text-based header detectors that run BEFORE the lexer/parser
// see the source: the struct-form rewriter (rewriter.go), the
// legacy-procedural rejection gate, and the per-construct function
// slicer (component/memql/function_slices.go).
//
// Those detectors match `func (Receiver) ...` / `<kind> <name> {`
// headers with line-anchored regexes and brace-depth scans. A
// procedural `func (Query)` token that appears inside a `// ...` line
// comment or a `/* ... */` block comment is NOT a real header, but the
// naive scanners treated it as one and then mis-parsed the FOLLOWING
// struct construct (memql#1074). BlankComments replaces the BYTES of
// every comment with spaces (newlines preserved) so the detectors scan
// a comment-free view whose byte offsets and line numbers still line up
// 1:1 with the original source. Callers detect against the blanked copy
// and splice/slice against the original, so authored comments survive
// in the emitted output.

// BlankComments returns a copy of source in which every `//` line
// comment and `/* ... */` block comment has its content replaced by
// spaces. Newlines inside block comments are preserved so line numbers
// are unchanged, and the returned string has exactly the same length as
// the input so byte offsets remain valid. Comment markers that appear
// inside a double-quoted string literal are left untouched, mirroring
// the tokenizer: a `"// not a comment"` literal keeps its `//`.
//
// Escape handling inside strings matches the brace scanners
// (findMatchingCloseBraceRune / braceDepthBefore): a backslash escapes
// the next byte. Raw backtick strings are treated as string literals
// too (the rewriter emits backtick-wrapped expressions), so a `//`
// inside a backtick span is preserved.
func BlankComments(source string) string {
	out := []byte(source)
	n := len(out)
	const (
		stateCode = iota
		stateString
		stateBacktick
		stateLineComment
		stateBlockComment
	)
	state := stateCode
	for i := 0; i < n; i++ {
		c := out[i]
		switch state {
		case stateString:
			if c == '\\' && i+1 < n {
				i++ // skip escaped byte
				continue
			}
			if c == '"' {
				state = stateCode
			}
		case stateBacktick:
			if c == '`' {
				state = stateCode
			}
		case stateLineComment:
			if c == '\n' {
				state = stateCode
				continue // keep the newline
			}
			out[i] = ' '
		case stateBlockComment:
			if c == '*' && i+1 < n && out[i+1] == '/' {
				out[i] = ' '
				out[i+1] = ' '
				i++
				state = stateCode
				continue
			}
			if c != '\n' {
				out[i] = ' '
			}
		default: // stateCode
			switch {
			case c == '"':
				state = stateString
			case c == '`':
				state = stateBacktick
			case c == '/' && i+1 < n && out[i+1] == '/':
				out[i] = ' '
				out[i+1] = ' '
				i++
				state = stateLineComment
			case c == '/' && i+1 < n && out[i+1] == '*':
				out[i] = ' '
				out[i+1] = ' '
				i++
				state = stateBlockComment
			}
		}
	}
	return string(out)
}

// UnterminatedBlockCommentLine returns the 1-based line on which an
// unterminated `/*` opens, or 0 when every block comment in source is closed.
//
// It shares BlankComments' state machine, so it agrees with it exactly on what
// counts as a comment opener: a `/*` inside a string, a backtick span, or a
// `//` line comment is not one.
//
// The motivating case is memql#2861. An unterminated block comment comments
// out the rest of the file -- that is what the lexer does, and what the
// comment-blanked header detectors now do -- so every construct below it goes
// ABSENT. Silently absent is exactly what memql#2830 outlawed for automations:
// one typo'd `/*` removes a workflow from the fleet with no diagnostic. The
// input is lexer-legal, so this is a WARN-grade signal, not a load failure;
// callers decide.
func UnterminatedBlockCommentLine(source string) int {
	const (
		stateCode = iota
		stateString
		stateBacktick
		stateLineComment
		stateBlockComment
	)
	state := stateCode
	n := len(source)
	openedAt := -1
	for i := 0; i < n; i++ {
		c := source[i]
		switch state {
		case stateString:
			if c == '\\' && i+1 < n {
				i++
				continue
			}
			if c == '"' {
				state = stateCode
			}
		case stateBacktick:
			if c == '`' {
				state = stateCode
			}
		case stateLineComment:
			if c == '\n' {
				state = stateCode
			}
		case stateBlockComment:
			if c == '*' && i+1 < n && source[i+1] == '/' {
				i++
				state = stateCode
				openedAt = -1
			}
		default: // stateCode
			switch {
			case c == '"':
				state = stateString
			case c == '`':
				state = stateBacktick
			case c == '/' && i+1 < n && source[i+1] == '/':
				i++
				state = stateLineComment
			case c == '/' && i+1 < n && source[i+1] == '*':
				openedAt = i
				i++
				state = stateBlockComment
			}
		}
	}
	if state == stateBlockComment && openedAt >= 0 {
		return strings.Count(source[:openedAt], "\n") + 1
	}
	return 0
}

// CommentSpans returns the byte ranges [Start, End) of every comment in
// source, in ascending order. It shares BlankComments' state machine, so it
// agrees with it exactly on what counts as a comment: a `//` or `/*` inside a
// string literal or a backtick span is not one.
//
// Why this exists when BlankComments already marks comments (memql#2872).
// Blanking is not enough to answer "is this line inside a comment", because a
// BLANK LINE INSIDE a block comment is byte-identical in both views --
// newlines are preserved and there is nothing else on the line to blank. A
// backwards preamble walk that only compares the two views therefore stops
// dead in the middle of a block comment, and the slice it cuts starts
// mid-comment: the remaining comment text and its `*/` are then parsed as
// code. That is the documented "2+ consecutive blank lines refuses boot"
// failure that sank two earlier attempts at #2872.
//
// A caller that needs "does this offset sit inside a comment" needs spans, not
// a blanked copy.
func CommentSpans(source string) []CommentSpan {
	var out []CommentSpan
	const (
		stateCode = iota
		stateString
		stateBacktick
		stateLineComment
		stateBlockComment
	)
	state := stateCode
	n := len(source)
	start := 0
	for i := 0; i < n; i++ {
		c := source[i]
		switch state {
		case stateString:
			if c == '\\' && i+1 < n {
				i++
				continue
			}
			if c == '"' {
				state = stateCode
			}
		case stateBacktick:
			if c == '`' {
				state = stateCode
			}
		case stateLineComment:
			if c == '\n' {
				out = append(out, CommentSpan{Start: start, End: i})
				state = stateCode
			}
		case stateBlockComment:
			if c == '*' && i+1 < n && source[i+1] == '/' {
				i++
				out = append(out, CommentSpan{Start: start, End: i + 1})
				state = stateCode
			}
		default: // stateCode
			switch {
			case c == '"':
				state = stateString
			case c == '`':
				state = stateBacktick
			case c == '/' && i+1 < n && source[i+1] == '/':
				start = i
				i++
				state = stateLineComment
			case c == '/' && i+1 < n && source[i+1] == '*':
				start = i
				i++
				state = stateBlockComment
			}
		}
	}
	// An unterminated comment runs to EOF -- that is what the lexer does, and
	// what UnterminatedBlockCommentLine reports on.
	if state == stateLineComment || state == stateBlockComment {
		out = append(out, CommentSpan{Start: start, End: n})
	}
	return out
}

// CommentSpan is a half-open byte range [Start, End) covering one comment,
// including its delimiters.
type CommentSpan struct{ Start, End int }

// OffsetInComment reports whether off lies inside one of spans. spans must be
// ascending, as returned by CommentSpans.
func OffsetInComment(spans []CommentSpan, off int) bool {
	_, ok := CommentSpanContaining(spans, off)
	return ok
}

// CommentSpanContaining returns the span containing off, if any. spans must be
// ascending, as returned by CommentSpans.
//
// Binary search: the ascending contract is documented, so a linear scan just
// wastes it.
func CommentSpanContaining(spans []CommentSpan, off int) (CommentSpan, bool) {
	lo, hi := 0, len(spans)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case off < spans[mid].Start:
			hi = mid - 1
		case off >= spans[mid].End:
			lo = mid + 1
		default:
			return spans[mid], true
		}
	}
	return CommentSpan{}, false
}
