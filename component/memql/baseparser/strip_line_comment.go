package baseparser

// strip_line_comment.go -- memql#3120.
//
// StripLineComment consolidates three byte-identical copies that lived in
// component/language/pagination, component/memql and dsl/, each carrying the
// same defect: deciding whether a `"` closed a string literal by looking ONE
// BYTE BACK for a backslash.
//
// That test cannot distinguish an escaped quote (`\"`) from a quote that
// follows a COMPLETED escape (`\\"`). On a literal ending in a backslash pair
// the scanner reads the real closing quote as escaped, never leaves string
// state, and treats everything after it as literal interior -- so a trailing
// `// ...` comment is not stripped and whatever consumes the line sees comment
// text as code.
//
// Escape state is TRACKED here, never inferred from the preceding byte, which
// is the same shape as the fixes in memql#2949, memql#2872, memql#3045 and
// memql#3046. Those landed one site at a time because nobody had listed the
// rest; memql#3120 listed them, and this file is where the three that share a
// single semantics stop being three.

// StripLineComment trims a trailing `// ...` line comment, preserving any `//`
// that appears inside a double-quoted string literal.
//
// Scope, deliberately narrow to match every caller it replaces:
//
//   - `"` only. Single quotes are not string delimiters in the sources these
//     callers scan, and treating them as such would change what a line means.
//   - Line comments only. A `/* ... */` on the line is left alone; use
//     BlankComments when block comments matter.
//   - Operates per line. A string literal spanning a newline is not this
//     function's problem, because a caller that has already split by line
//     cannot see one.
func StripLineComment(line string) string {
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]

		if inString {
			// Consume the escaped byte with the backslash, so `\"` cannot
			// close the string and `\\` cannot make the NEXT quote look
			// escaped. This single step is the whole fix.
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			continue
		}
		if c == '/' && i+1 < len(line) && line[i+1] == '/' {
			return line[:i]
		}
	}
	return line
}
