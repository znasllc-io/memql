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

// A backtick fence's info string cannot hold a backtick, so this line opens
// no fence: it is a code span in a paragraph. Reading it as a fence masked the
// rest of the page, and every link after it went unchecked.
func TestBacktickInfoStringOpensNoFence(t *testing.T) {
	content := "```inline``` is a code span, not a fence.\n\nSee [the design](../../internal/x.md).\n"
	if got := targets(content); len(got) != 1 || got[0] != "../../internal/x.md" {
		t.Errorf("targets = %q: the line was read as a fence", got)
	}
	// A tilde fence's info string may hold backticks.
	if got := targets("~~~ `x`\n[a](../n.md)\n~~~\n"); len(got) != 0 {
		t.Errorf("targets = %q: a tilde fence was not read", got)
	}
	if fenceOpens("```go") != "```" || fenceOpens("```go `x`") != "" || fenceOpens("~~~ `x`") != "~~~" {
		t.Error("fenceOpens disagrees with CommonMark on info strings")
	}
}

// An indented code block is code, but only where CommonMark reads one: after
// a blank line, and not where a list item owns the indent.
func TestMaskIndentedCode(t *testing.T) {
	for content, want := range map[string]string{
		"Para.\n\n    [a](../n.md)\n\nText [b](../y.md).": "../y.md",
		"    [a](../n.md)\n\n    [b](../n.md)\nText.":     "",
		"\tcode [a](../n.md)\n":                           "",
		"- item\n\n    [a](../y.md)":                      "../y.md",
		"1. step\n\n    [a](../y.md)":                     "../y.md",
		"[^1]: note\n\n    [a](../y.md)":                  "../y.md",
		"Para\n    [a](../y.md)":                          "../y.md",
		"- item\n\nPara.\n\n    [a](../n.md)":             "",
		"Para.\n\n    ```\n[a](../y.md)":                  "../y.md",
		"# Title\n    [a](../n.md)":                       "",
		"* * *\n    [a](../n.md)":                         "",
		"```\nx\n```\n    [a](../n.md)":                   "",
	} {
		if got := strings.Join(targets(content), " "); got != want {
			t.Errorf("%q: read %q, want %q", content, got, want)
		}
	}
}

// A code span runs across the lines of its paragraph, and no further: not
// past a blank line, and not into a block that interrupts the paragraph.
func TestMaskCodeSpanAcrossLines(t *testing.T) {
	for content, want := range map[string]string{
		"Use `foo\n[x](../n.md)` here.":                            "",
		"Use `foo\nbar` and [x](../y.md) and `baz`.":               "../y.md",
		"Use `foo\n\n[x](../y.md)\n\nand ` here.":                  "../y.md",
		"Use `a\n- item [x](../y.md) and ` b":                      "../y.md",
		"Use `a\n# Head [x](../y.md) ` b":                          "../y.md",
		"> Use `foo\n> bar` and [x](../y.md) and `baz`.":           "../y.md",
		"| h | h | h |\n|---|---|---|\n| `a | [x](../y.md) | b` |": "../y.md",
		"| h |\n|---|\n| `a \\| [x](../n.md) b` |":                 "",
	} {
		if got := strings.Join(targets(content), " "); got != want {
			t.Errorf("%q: read %q, want %q", content, got, want)
		}
	}
	in := "Use `foo\n[x](../n.md)` here."
	if got := Mask(in); strings.Contains(got, "../n.md") || len(got) != len(in) || strings.Count(got, "\n") != 1 {
		t.Errorf("Mask(%q) = %q", in, got)
	}
}

func TestStripHTMLCommentsReadsCodeAsLinksDo(t *testing.T) {
	for in, want := range map[string]string{
		// A comment inside a code span that crosses lines is content.
		"A `span\nstill <!-- kept --> code` stays.\n": "A `span\nstill <!-- kept --> code` stays.\n",
		// A line that only looks like a fence does not protect what follows.
		"```x``` then <!-- gone --> here.\n": "```x``` then  here.\n",
		// `<!-->` is a whole, empty comment (CommonMark 0.31).
		"a <!--> b\n": "a  b\n",
		// Indented code is content.
		"Para.\n\n    <!-- kept -->\n": "Para.\n\n    <!-- kept -->\n",
		// An escaped opener is text.
		"a \\<!-- b --> c\n": "a \\<!-- b --> c\n",
	} {
		if got := StripHTMLComments(in); got != want {
			t.Errorf("StripHTMLComments(%q) = %q, want %q", in, got, want)
		}
	}
}
