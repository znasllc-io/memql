package sense

import "testing"

// tokenize_string_escape_test.go -- memql#3190, the sense tokenizer's
// string-position heuristic.
//
// isInsideString decided where a literal ended with a ONE-BYTE LOOKBACK
// (`line[i-1] != '\\'`), which cannot tell an escaped quote from a quote that
// follows a COMPLETED `\\` escape. A line whose literal ends in a backslash
// pair therefore never left string state, so every `//` and `/*` AFTER that
// literal was reported as being inside a string and no comment token was
// emitted for it -- a comment silently losing its highlighting in the editor,
// for the rest of the line.

func TestIsInsideString_CompletedEscapeClosesTheLiteral(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		pos  int
		want bool
	}{
		{
			// The `//` after a literal ending in `\\` is real code, not
			// literal interior.
			name: "after a literal ending in a completed backslash escape",
			line: `path: "C:\\" // trailing comment`,
			pos:  13,
			want: false,
		},
		{
			name: "inside a literal ending in a completed backslash escape",
			line: `path: "C:\\" // trailing comment`,
			pos:  8,
			want: true,
		},
		{
			// An escaped quote must still NOT close the literal.
			name: "inside a literal containing an escaped quote",
			line: `path: "he said \" here" // trailing comment`,
			pos:  20,
			want: true,
		},
		{
			name: "after a literal containing an escaped quote",
			line: `path: "he said \" here" // trailing comment`,
			pos:  24,
			want: false,
		},
		{
			name: "plain literal, position after it",
			line: `path: "plain" // trailing comment`,
			pos:  14,
			want: false,
		},
		{
			name: "plain literal, position inside it",
			line: `path: "plain" // trailing comment`,
			pos:  9,
			want: true,
		},
		{
			name: "no literal at all",
			line: `path: value // trailing comment`,
			pos:  12,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInsideString(tc.line, tc.pos); got != tc.want {
				t.Errorf("isInsideString(%q, %d) = %v, want %v (byte at pos: %q)",
					tc.line, tc.pos, got, tc.want, tc.line[tc.pos:])
			}
		})
	}
}

// The whole-tokenizer effect: the trailing comment on a line whose literal
// ends in `\\` must still be emitted as a comment token.
func TestExtractComments_CommentAfterCompletedEscapeLiteral(t *testing.T) {
	source := `concept x {
  path string @default("C:\\") // this is a comment
}`
	var found bool
	for _, tok := range extractComments(source) {
		if tok.Type == "comment" && tok.Literal == "// this is a comment" {
			found = true
		}
	}
	if !found {
		t.Errorf("the trailing comment after a literal ending in `\\\\` was not tokenized as a comment -- "+
			"the string scan treats the closing quote as escaped, so the rest of the line reads as literal interior.\nsource:\n%s\ntokens: %#v",
			source, extractComments(source))
	}
}
