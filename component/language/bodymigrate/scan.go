package bodymigrate

// scan.go -- the text scanning the bodies rewrite reads the retired
// body forms with (epic memql#5370, task memql#5373).
//
// THE REWRITE READS TEXT, NOT THE ENGINE'S PARSE. The retired forms are the
// ones the engine stops parsing in this same epic, and a bundle repository
// runs this rewrite against whatever engine it has installed -- so the reader
// cannot lean on the procedural parser the flip deletes. It also has to keep
// every comment where the author put it, which no parse of this grammar does.
//
// The technique is the one parser/rewriter.go uses: locate structure on a
// VIEW of the source with comment and string contents blanked (byte offsets
// preserved, newlines kept), then slice the original text at those offsets.
// A brace inside a string or a comment is therefore never structure.

import (
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// codeView blanks comments and string contents, preserving every offset and
// newline. Quote characters survive, so a string's extent is visible.
func codeView(src string) string { return langparser.BlankCommentsAndStrings(src) }

// matchClose returns the index of the bracket that closes the one at open,
// scanning a code view, or -1 when it is unbalanced or crossed.
func matchClose(view string, open int) int {
	if open < 0 || open >= len(view) {
		return -1
	}
	var want byte
	switch view[open] {
	case '(':
		want = ')'
	case '[':
		want = ']'
	case '{':
		want = '}'
	default:
		return -1
	}
	var stack []byte
	for i := open; i < len(view); i++ {
		switch c := view[i]; c {
		case '(':
			stack = append(stack, ')')
		case '[':
			stack = append(stack, ']')
		case '{':
			stack = append(stack, '}')
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return -1
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				if c != want {
					return -1
				}
				return i
			}
		}
	}
	return -1
}

// splitTopLevel splits raw at every sep that sits at bracket depth zero in
// view (view and raw have the same length). The pieces are raw text,
// untrimmed.
func splitTopLevel(raw, view string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(view); i++ {
		switch view[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		default:
			if view[i] == sep && depth == 0 {
				out = append(out, raw[start:i])
				start = i + 1
			}
		}
	}
	return append(out, raw[start:])
}

// firstTopLevel returns the index of the first byte c at depth zero in view,
// or -1.
func firstTopLevel(view string, c byte) int {
	depth := 0
	for i := 0; i < len(view); i++ {
		switch view[i] {
		case '(', '[', '{':
			if view[i] == c && depth == 0 {
				return i
			}
			depth++
		case ')', ']', '}':
			depth--
		default:
			if view[i] == c && depth == 0 {
				return i
			}
		}
	}
	return -1
}

// isIdentByte reports whether c can continue an identifier.
func isIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isIdentStartByte reports whether c can begin an identifier.
func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// leadingIdent splits s (already left-trimmed) into its leading identifier and
// the rest. The identifier is "" when s does not start with one.
func leadingIdent(s string) (string, string) {
	if s == "" || !isIdentStartByte(s[0]) {
		return "", s
	}
	i := 1
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

// isBareIdent reports whether s is exactly one identifier.
func isBareIdent(s string) bool {
	id, rest := leadingIdent(s)
	return id != "" && rest == ""
}

// lineAt returns the 1-based line of offset off in src.
func lineAt(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return strings.Count(src[:off], "\n") + 1
}

// indentOf returns the leading spaces and tabs of line.
func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// continuesAtLineEnd reports whether a line of code (a view line, trimmed)
// cannot end a statement because it ends on an operator or a separator.
func continuesAtLineEnd(line string) bool {
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return false
	}
	for _, op := range []string{"&&", "||", "??", "==", "!=", "<=", ">=", "=>", ":="} {
		if strings.HasSuffix(line, op) {
			return true
		}
	}
	switch line[len(line)-1] {
	case '+', '-', '*', '/', '%', '<', '>', '?', ':', ',', '.', '!', '=', '(', '[', '{':
		return true
	}
	// `in` / `startsWith` as a trailing operator word.
	for _, w := range []string{" in", " startsWith"} {
		if strings.HasSuffix(line, w) {
			return true
		}
	}
	return false
}

// continuesAtLineStart reports whether a line of code (a view line, trimmed)
// continues the statement above it: no statement begins with an operator, a
// closing bracket, or `else`.
func continuesAtLineStart(line string) bool {
	line = strings.TrimLeft(line, " \t")
	if line == "" {
		return false
	}
	for _, op := range []string{"&&", "||", "??", "==", "!=", "<=", ">=", "=>"} {
		if strings.HasPrefix(line, op) {
			return true
		}
	}
	switch line[0] {
	case '+', '*', '/', '%', '<', '>', '?', ':', '.', ')', ']', '}', ',':
		return true
	case '-':
		return true
	}
	if w, _ := leadingIdent(line); w == "else" || w == "in" || w == "startsWith" {
		return true
	}
	return false
}

// reindent moves every line of s after the first from an indentation base of
// `from` columns to a base of `to`, keeping each line's offset from the base.
// The first line is the caller's to place. Lines inside a multi-line string
// literal are left alone, so a literal's content never changes.
func reindent(s string, from, to int) string {
	if from == to {
		return s
	}
	lines := strings.Split(s, "\n")
	view := strings.Split(codeView(s), "\n")
	inString := false
	for i := range lines {
		if i > 0 && !inString {
			lines[i] = shiftLine(lines[i], from, to)
		}
		// Track whether the line ends inside a string literal: the view
		// keeps quote characters, so an odd count of unescaped quotes on
		// the view line flips the state.
		if i < len(view) && strings.Count(view[i], `"`)%2 == 1 {
			inString = !inString
		}
	}
	return strings.Join(lines, "\n")
}

// shiftLine moves one line's indentation from a base of `from` columns to a
// base of `to`, keeping its offset relative to the base. A line indented less
// than the base keeps whatever it has beyond column zero.
func shiftLine(line string, from, to int) string {
	if strings.TrimSpace(line) == "" {
		return ""
	}
	ind := indentOf(line)
	width := len(strings.ReplaceAll(ind, "\t", "    "))
	rest := line[len(ind):]
	newWidth := width - from + to
	if newWidth < 0 {
		newWidth = 0
	}
	return strings.Repeat(" ", newWidth) + rest
}

// trailingComment splits a single line into its code and a trailing `//`
// comment that sits outside any string, returning the comment with its `//`.
// A line that is a string literal's continuation is not handed here.
func trailingComment(line string) (code, comment string) {
	inStr := false
	for i := 0; i+1 < len(line); i++ {
		c := line[i]
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
		if c == '"' {
			inStr = true
			continue
		}
		if c == '/' && line[i+1] == '/' {
			return strings.TrimRight(line[:i], " \t"), strings.TrimSpace(line[i:])
		}
	}
	return line, ""
}

// isCommentLine reports whether a line holds only a comment.
func isCommentLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*")
}
