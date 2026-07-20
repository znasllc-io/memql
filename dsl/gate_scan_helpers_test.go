package dsl

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

// TestArgsFieldDescriptionLines_PositionAware pins the #2658 findings:
// an annotation on the `args {` opener line IS inside the block (the
// old line scanner skipped the opener), and a one-line `} shape {`
// transition exits args at the closing brace -- the sibling block's
// load-bearing field prose never flags.
func TestArgsFieldDescriptionLines_PositionAware(t *testing.T) {
	opener := "query q {\n  args { x string @description(\"dead\") }\n}\n"
	if got := argsFieldDescriptionLines(opener); len(got) != 1 || got[0] != 2 {
		t.Errorf("opener-line annotation: want line 2, got %v", got)
	}

	transition := "mutation m {\n  args {\n    x string\n  } shape {\n    y string @description(\"keep\")\n  }\n}\n"
	if got := argsFieldDescriptionLines(transition); len(got) != 0 {
		t.Errorf("one-line }-shape-{ transition: want no hits, got %v", got)
	}
}
