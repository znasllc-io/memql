package dslconformance

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/sense"
)

// The gates' lexical scanning is shared with the sense editor rules
// (component/memql/sense): one implementation, hinted in the editor
// and enforced in CI, so the two cannot drift. The tests here pin the
// shared behavior from the gate's side.

// blankBlockComments delegates to the shared state-machine blanker. A
// bare regex cannot do this job: a `/*` inside a line comment or a
// string literal must NOT open a span -- combined with a real */
// downstream it phantom-blanks live corpus (#2615, #2658 review).
func blankBlockComments(src string) string {
	return sense.BlankBlockComments(src)
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
			// The discriminating shape (#2658 review): with a REAL
			// span downstream, a naive regex pairs the // glob with
			// its closer and phantom-blanks the live code between.
			name: "line-comment glob plus real span downstream",
			in:   "// maps scripts/* path\nargs {\n/* real */\n",
			want: "// maps scripts/* path\nargs {\n          \n",
		},
		{
			name: "string glob plus real span downstream",
			in:   "x string @pattern(\"/*\")\nargs {\n/* real */\n",
			want: "x string @pattern(\"/*\")\nargs {\n          \n",
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

// (The args-field @description line scanner these helpers also pinned is
// gone -- memql#3336 made the annotation a parse rejection, so the corpus is
// gated structurally: a .memql carrying one refuses to load, which
// dslimports.Load in server_only_parsed_test.go already fails on. The
// regex gate that used to stand in for that is retired with the scanner.)
