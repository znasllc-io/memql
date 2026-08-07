package baseparser

import "testing"

// strip_line_comment_test.go -- memql#3190, the consolidated line-comment
// stripper.
//
// StripLineComment replaces three byte-identical private copies
// (pagination.stripComment, memql.stripDepLineComment and the DSL conformance
// suite's stripLineComment). All three decided where a literal ended with a
// ONE-BYTE LOOKBACK (`line[i-1] != '\\'`), which cannot tell an escaped quote
// from a quote that follows a COMPLETED `\\` escape: on
//
//	x: "C:\\" // note
//
// each of the three returned the line UNCHANGED, comment and all, because the
// closing quote read as escaped and the `//` counted as literal interior. The
// callers then classified comment text as authored DSL. Measured against all
// three before the consolidation; the completed-escape case below is the one
// that bit.
func TestStripLineComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "literal ending in a completed backslash escape",
			in:   `x: "C:\\" // note`,
			want: `x: "C:\\" `,
		},
		{
			name: "two completed escapes in a row",
			in:   `x: "a\\\\" // note`,
			want: `x: "a\\\\" `,
		},
		{
			name: "escaped quote does not close the literal",
			in:   `x: "he said \" // not a comment" // note`,
			want: `x: "he said \" // not a comment" `,
		},
		{
			name: "comment inside a literal is preserved",
			in:   `x: "http://example.com"`,
			want: `x: "http://example.com"`,
		},
		{
			name: "plain trailing comment",
			in:   `filter status=="active" // only active rows`,
			want: `filter status=="active" `,
		},
		{
			name: "whole-line comment",
			in:   `  // just a comment`,
			want: `  `,
		},
		{
			name: "no comment",
			in:   `filter status=="active"`,
			want: `filter status=="active"`,
		},
		{
			name: "empty line",
			in:   ``,
			want: ``,
		},
		{
			name: "single slash is not a comment",
			in:   `x: a/b`,
			want: `x: a/b`,
		},
		{
			name: "unterminated literal swallows the rest of the line",
			in:   `x: "unterminated // note`,
			want: `x: "unterminated // note`,
		},
		{
			// A backslash in CODE escapes nothing -- the `"` after it opens a
			// literal, exactly as the lexer and BlankComments read it. The
			// three lookback copies instead saw both quotes as escaped, never
			// entered string state, and stripped at the `//`. This spelling is
			// not legal MemQL either way; the case is here to pin which of the
			// two readings this function follows.
			name: "a backslash in code does not suppress the quote that follows it",
			in:   `x: \"quoted\" // note`,
			want: `x: \"quoted\" // note`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripLineComment(tc.in); got != tc.want {
				t.Errorf("StripLineComment(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// StripLineComment agrees with BlankComments on where a literal ends: both
// TRACK escape state rather than looking one byte back. Drifting apart is the
// failure this consolidation exists to prevent, so it is pinned.
func TestStripLineCommentAgreesWithBlankComments(t *testing.T) {
	for _, line := range []string{
		`x: "C:\\" // note`,
		`x: "he said \" // not a comment" // note`,
		`x: "http://example.com"`,
		`filter status=="active" // only active rows`,
	} {
		blanked := BlankComments(line)
		stripped := StripLineComment(line)
		// Everything StripLineComment kept must survive BlankComments
		// unchanged: it is exactly the non-comment prefix.
		if len(stripped) > len(blanked) {
			t.Fatalf("StripLineComment(%q) is longer than the line", line)
		}
		if blanked[:len(stripped)] != stripped {
			t.Errorf("disagreement on %q:\n StripLineComment: %q\n BlankComments:    %q",
				line, stripped, blanked)
		}
	}
}
