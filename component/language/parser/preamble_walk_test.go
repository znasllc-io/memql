package parser

import (
	"regexp"
	"strings"
	"testing"
)

// The review's shape (memql#5426): @level above an annotation whose argument
// list spans lines. The walk stopped at `)` and cut @level out of the slice.
const multiLinePromptSource = `concept other {
  x string
}

@level("fast")
@description(
  "Summarise a ticket."
)
@templateFile("summ.tmpl")
prompt acmeSumm {
  ticket object
}
`

// TestPreambleStartOfReadsAMultiLineAnnotationWhole: the preamble starts at
// the first annotation above the header, through a multi-line one.
func TestPreambleStartOfReadsAMultiLineAnnotationWhole(t *testing.T) {
	header := strings.Index(multiLinePromptSource, "prompt acmeSumm")
	want := strings.Index(multiLinePromptSource, `@level("fast")`)
	if got := PreambleStartOf(multiLinePromptSource, header); got != want {
		t.Fatalf("preamble starts at %q, want it at @level:\n%s",
			firstLine(multiLinePromptSource[got:]), multiLinePromptSource[got:header])
	}
}

// TestPreambleContinuationSkipsStringsAndComments: a `)` inside a string, a raw
// string or a comment does not close the list, and brackets nest.
func TestPreambleContinuationSkipsStringsAndComments(t *testing.T) {
	src := "@rowAuthz(owner=\"ownerUserId\")\n" +
		"@handler(\n" +
		"  type=\"query\",\n" +
		"  query=\"paginate(query x(a: args.a), 10)\" // a ) in a comment\n" +
		"  , note=`raw ) text\n" +
		"  spanning a line`,\n" +
		"  list=[(1), (2)]\n" +
		")\n" +
		"tool probe {\n}\n"
	header := strings.Index(src, "tool probe")
	if got := PreambleStartOf(src, header); got != 0 {
		t.Fatalf("preamble starts at %q, want the first line:\n%s", firstLine(src[got:]), src)
	}
}

// TestPreambleTreatsAMultiLineAnnotationLikeAOneLineOne: spreading an
// annotation over lines changes nothing else about the walk -- a blank line
// directly above the header still detaches it, an empty line between two
// annotations is still stepped over, and a comment line still belongs.
func TestPreambleTreatsAMultiLineAnnotationLikeAOneLineOne(t *testing.T) {
	const one = `@description("x")`
	const multi = "@description(\n  \"x\"\n)"
	layouts := map[string]string{
		"blank line above the header": "%s\n\nprompt p {\n}\n",
		"empty line between":          "@level(\"fast\")\n\n%s\nprompt p {\n}\n",
		"empty line below":            "%s\n\n@level(\"fast\")\nprompt p {\n}\n",
		"comment between":             "@level(\"fast\")\n// note\n%s\n// note\nprompt p {\n}\n",
	}
	for name, layout := range layouts {
		t.Run(name, func(t *testing.T) {
			a := strings.Replace(layout, "%s", one, 1)
			b := strings.Replace(layout, "%s", multi, 1)
			startA := PreambleStartOf(a, strings.Index(a, "prompt p"))
			startB := PreambleStartOf(b, strings.Index(b, "prompt p"))
			// Compared by the annotation each starts at, since the one
			// spread over lines has a different first line.
			if opening(a[startA:]) != opening(b[startB:]) {
				t.Fatalf("one-line preamble starts at %q, multi-line at %q", firstLine(a[startA:]), firstLine(b[startB:]))
			}
		})
	}
}

// TestPreambleIgnoresAnUnclosedAnnotation: a list that never closes records no
// continuation, so the NEXT declaration's preamble does not swallow the lines
// below it -- one malformed annotation stays one broken declaration.
func TestPreambleIgnoresAnUnclosedAnnotation(t *testing.T) {
	src := "@description(\"never closed\"\nquery thing a {\n  filter row => row.x == 1\n}\n@public\nquery thing b {\n}\n"
	header := strings.Index(src, "query thing b")
	want := strings.Index(src, "@public")
	if got := PreambleStartOf(src, header); got != want {
		t.Fatalf("preamble of b starts at %q, want it at @public", firstLine(src[got:]))
	}
}

// TestPreambleIgnoresAnInlineFieldAnnotation: a multi-line annotation on a
// field line inside a body is not a preamble annotation, so a walk that
// reaches its closing line does not climb into the body above.
func TestPreambleIgnoresAnInlineFieldAnnotation(t *testing.T) {
	src := "concept a {\n  x string @description(\n    \"field\"\n  ) }\nquery a b {\n}\n"
	header := strings.Index(src, "query a b")
	if got := PreambleStartOf(src, header); got != header {
		t.Fatalf("preamble of b starts at %q, want the header itself", firstLine(src[got:]))
	}
}

// TestDeclarationSliceKeepsAnnotationsAboveAMultiLineAnnotation: the slice a
// loader parses carries @rowAuthz above a multi-line @description -- without
// it the concept loads on the undeclared row-authz tier.
func TestDeclarationSliceKeepsAnnotationsAboveAMultiLineAnnotation(t *testing.T) {
	src := "@rowAuthz(owner=\"ownerUserId\")\n@description(\n  \"A ticket.\"\n)\nconcept ticket {\n  ownerUserId string\n}\n"
	slices := ExtractDeclarationSlices(src, regexp.MustCompile(`(?m)^concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`))
	if len(slices) != 1 {
		t.Fatalf("want one slice, got %d", len(slices))
	}
	if !strings.HasPrefix(slices[0].Source, "@rowAuthz(") {
		t.Fatalf("slice lost @rowAuthz:\n%s", slices[0].Source)
	}
}

// TestOrphanedPreambleReportsARunEndingInAMultiLineAnnotation: a block comment
// that orphans a run whose last annotation spans lines is reported, with the
// run's first annotation.
func TestOrphanedPreambleReportsARunEndingInAMultiLineAnnotation(t *testing.T) {
	src := "@level(\"fast\")\n@description(\n  \"x\"\n)\n/* note */\nprompt p {\n}\n"
	got := OrphanedPreambles(src)
	if len(got) != 1 {
		t.Fatalf("want one orphaned run, got %d", len(got))
	}
	if got[0].Line != 1 || !strings.HasPrefix(got[0].Attributes, `@level("fast")`) {
		t.Fatalf("orphaned run at line %d = %q, want it to open at @level on line 1", got[0].Line, got[0].Attributes)
	}
}

// TestContinuationLinesIndexesLines: the line-indexed view agrees with the
// offset one.
func TestContinuationLinesIndexesLines(t *testing.T) {
	src := "@a(\n  1,\n  2\n)\nquery x y {\n}\n"
	got := ContinuationLines(src)
	want := map[int]int{1: 0, 2: 0, 3: 0}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// opening is the text of s up to its first `(` or newline: the annotation a
// preamble starts at, or the header when it starts at the header.
func opening(s string) string {
	if i := strings.IndexAny(s, "(\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
