package docsmd

import (
	"strings"
	"testing"
)

// TestStripHTMLCommentsMatchesTheBundlerFixture is the fixture the Python
// bundler was pinned by on the docs-readers branch (docs_bundle_comments_test.go,
// memql#5721), input and expected output unchanged, so the Go bundler that
// replaces it strips exactly what that one did.
func TestStripHTMLCommentsMatchesTheBundlerFixture(t *testing.T) {
	source := strings.Join([]string{
		"---",
		"title: Fixture",
		"audience: public",
		"area: operate",
		"---",
		"",
		"Intro paragraph.",
		"<!-- corpus: 2026/examples/fixture.memql -->",
		"```memql",
		"<!-- inside a fence, this is content -->",
		"```",
		"",
		"The label `portal` is reserved. <!-- retired-vocabulary-ok: a reason -->",
		"A span `<!-- kept -->` stays, and <!-- gone --> goes.",
		"| a | b <!-- proving-pending: metric=x --> | c |",
		"Before.",
		"<!-- BEGIN GENERATED",
		"spanning lines",
		"-->",
		"After.",
		"",
	}, "\n")
	want := strings.Join([]string{
		"---",
		"title: Fixture",
		"audience: public",
		"area: operate",
		"---",
		"",
		"Intro paragraph.",
		"",
		"```memql",
		"<!-- inside a fence, this is content -->",
		"```",
		"",
		"The label `portal` is reserved.",
		"A span `<!-- kept -->` stays, and  goes.",
		"| a | b  | c |",
		"Before.",
		"",
		"After.",
		"",
	}, "\n")
	if got := StripHTMLComments(source); got != want {
		t.Errorf("stripped markdown differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A comment opener alone on its line, the usual way to open a multi-line
// comment. The Python bundler kept that line verbatim -- the opener was the
// line's last four bytes, so its loop ended before marking anything removed --
// and shipped a dangling `<!--`. This one leaves the blank line the comment
// made.
func TestStripHTMLCommentsRemovesABareOpenerLine(t *testing.T) {
	got := StripHTMLComments("Before.\n<!--\nhidden\n-->\nAfter.\n")
	if want := "Before.\n\nAfter.\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "<!--") {
		t.Error("a comment opener reached the output")
	}
}

func TestStripHTMLCommentsLeavesCodeAndCommentFreeTextAlone(t *testing.T) {
	cases := map[string]string{
		"no comment, CRLF":           "# T\r\n\r\nText with `code` and a [link](a.md).\r\n",
		"tilde fence":                "~~~\n<!-- in a fence -->\n~~~\n",
		"longer closing fence":       "```\n<!-- a -->\n`````\n",
		"unclosed fence":             "```\n<!-- a -->\nno close\n",
		"double-backtick code span":  "A ``span with ` and <!-- x -->`` stays.\n",
		"indented fence in a list":   "- item\n\n    ```\n    <!-- a -->\n    ```\n",
		"no trailing newline, clean": "last line",
	}
	for name, in := range cases {
		if got := StripHTMLComments(in); got != in {
			t.Errorf("%s: changed\n got %q\nwant %q", name, got, in)
		}
	}
}

func TestStripHTMLCommentsFenceCloseRules(t *testing.T) {
	// A shorter run, or the other character, does not close a fence, so the
	// comment after it is still content.
	in := "````\n```\n~~~~\n<!-- still code -->\n````\n<!-- gone -->\n"
	if got, want := StripHTMLComments(in), "````\n```\n~~~~\n<!-- still code -->\n````\n\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMaskKeepsOffsetsAndBlanksCodeAndComments(t *testing.T) {
	in := "a `[x](code.md)` b <!-- [y](c.md) --> [z](real.md)\n```\n[w](fenced.md)\n```\n"
	got := Mask(in)
	if len(got) != len(in) {
		t.Fatalf("mask changed the length: %d != %d", len(got), len(in))
	}
	for _, gone := range []string{"code.md", "c.md", "fenced.md", "<!--", "```"} {
		if strings.Contains(got, gone) {
			t.Errorf("mask kept %q", gone)
		}
	}
	i := strings.Index(in, "[z](real.md)")
	if got[i:i+len("[z](real.md)")] != "[z](real.md)" {
		t.Error("text outside code moved or changed")
	}
	if strings.Count(got, "\n") != strings.Count(in, "\n") {
		t.Error("line endings were not kept")
	}
}
