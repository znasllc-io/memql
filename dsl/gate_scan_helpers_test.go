package dsl

import (
	"strings"
	"testing"
)

// Shared lexical helpers for the tree gates. The gates scan raw .memql
// source (the constructs they police leave no AST trace), so they need
// comment- and string-aware blanking that a bare regex cannot provide:
// a `/*` inside a line comment or a string literal must NOT open a
// block-comment span (#2615 found dsl/deployment/actions.memql fully
// blanked by the `scripts/*` prose in a // comment).

// blankBlockComments blanks real /* */ spans (newline-preserving, so
// reported line numbers stay stable) while leaving line comments and
// string literals intact. String handling matches the per-line
// helpers: a bare `"` toggles, no escape processing.
func blankBlockComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	const (
		stNormal = iota
		stString
		stLineComment
		stBlockComment
	)
	state := stNormal
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case stNormal:
			switch {
			case c == '"':
				state = stString
				b.WriteByte(c)
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = stLineComment
				b.WriteByte(c)
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = stBlockComment
				b.WriteByte(' ')
				b.WriteByte(' ')
				i++
			default:
				b.WriteByte(c)
			}
		case stString:
			if c == '"' || c == '\n' {
				state = stNormal
			}
			b.WriteByte(c)
		case stLineComment:
			if c == '\n' {
				state = stNormal
			}
			b.WriteByte(c)
		case stBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = stNormal
				b.WriteByte(' ')
				b.WriteByte(' ')
				i++
			} else if c == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
	}
	return b.String()
}

// stripStringsForScan blanks string-literal contents and cuts // tails
// on a single line, so brace counting and annotation matching skip
// embedded syntax. Positions before the cut are preserved.
func stripStringsForScan(line string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if !inString && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			break
		}
		if c == '"' {
			inString = !inString
			b.WriteByte(' ')
			continue
		}
		if inString {
			b.WriteByte(' ')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestBlankBlockComments_RespectsLineCommentsAndStrings pins the #2615
// fix: `/*` inside a // comment or a string must not open a span, a
// real span blanks newline-preserving, and an unterminated span blanks
// to EOF.
func TestBlankBlockComments_RespectsLineCommentsAndStrings(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "line-comment glob does not open a span",
			in:   "// maps scripts/* path\nargs {\n",
			want: "// maps scripts/* path\nargs {\n",
		},
		{
			name: "string glob does not open a span",
			in:   "x string @pattern(\"/*\")\nargs {\n",
			want: "x string @pattern(\"/*\")\nargs {\n",
		},
		{
			name: "real span blanks newline-preserving",
			in:   "a\n/* two\nlines */\nb\n",
			want: "a\n      \n        \nb\n",
		},
		{
			name: "unterminated span blanks to EOF",
			in:   "a\n/* open\nrest",
			want: "a\n       \n    ",
		},
	}
	for _, tc := range cases {
		if got := blankBlockComments(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestArgsFieldDescriptionLines pins the args-block scanner: hits only
// inside args{}, not on declaration-level or concept-field annotations,
// and the deployment/actions.memql shape (a // comment containing /*
// upstream of the args block) stays visible.
func TestArgsFieldDescriptionLines(t *testing.T) {
	src := `@description("declaration level -- load-bearing")
// allowlisted scripts/* path prose
action probe {
  args {
    workdir string @required @description("discarded")
    ref     string
  }
}
concept widget {
  label string @description("concept field -- load-bearing")
}
`
	got := argsFieldDescriptionLines(src)
	if len(got) != 1 || got[0] != 5 {
		t.Errorf("want exactly line 5, got %v", got)
	}
}
