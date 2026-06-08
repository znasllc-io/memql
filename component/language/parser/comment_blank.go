package parser

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
