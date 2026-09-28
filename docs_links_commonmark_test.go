package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"

	"github.com/znasllc-io/memql/core/docsmd"
)

// The boundary gate and the bundle read a page's links with core/docsmd, a
// scanner written by hand because core stays stdlib-only (memql#5717). A link
// that scanner misses is a link the gate never checks and the bundle never
// rewrites, and the site renders it anyway: the first version read links with
// a one-line pattern that missed a titled link, `[t](../internal/x.md "T")`,
// and so shipped it.
//
// So the scanner is held to an independent CommonMark implementation --
// goldmark with the GFM extensions, the dialect the site renders -- over every
// page in docs/public and over the constructs that pattern got wrong. Every
// link and image goldmark renders must be one docsmd.Links returns. The
// converse is not required on real pages: reading a string as a link that
// CommonMark would not is a check that fails closed.

// commonmarkDestinations returns the destination of every link and image
// goldmark renders on page, reference-style uses resolved to their
// definition's destination.
func commonmarkDestinations(page []byte) []string {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	doc := md.Parser().Parse(text.NewReader(page))
	var out []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			out = append(out, string(v.Destination))
		case *ast.Image:
			out = append(out, string(v.Destination))
		}
		return ast.WalkContinue, nil
	})
	return out
}

// docsmdTargets returns the targets docsmd.Links reads on page: every inline
// link and image when inlineOnly, and every definition too otherwise.
func docsmdTargets(page string, inlineOnly bool) []string {
	var out []string
	for _, l := range docsmd.Links(page) {
		if inlineOnly && l.Definition {
			continue
		}
		out = append(out, l.Target)
	}
	return out
}

// missing returns the destinations in want that got does not hold.
func missing(want, got []string) []string {
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	var out []string
	for _, w := range want {
		if !have[w] {
			out = append(out, w)
		}
	}
	return out
}

func TestDocsmdSeesEveryLinkCommonMarkRendersInDocsPublic(t *testing.T) {
	files := gitTrackedDocsFiles(t)
	var pages, links int
	for _, rel := range files {
		if !strings.HasPrefix(filepath.ToSlash(rel), "docs/public/") {
			continue
		}
		data, err := os.ReadFile(rel)
		if err != nil {
			continue // tracked but absent in a partial checkout
		}
		pages++
		want := commonmarkDestinations(data)
		links += len(want)
		if miss := missing(want, docsmdTargets(string(data), false)); len(miss) > 0 {
			t.Errorf("%s: CommonMark renders links docsmd.Links does not read, so the boundary gate never checks them: %q", rel, miss)
		}
	}
	if pages == 0 || links == 0 {
		t.Fatalf("read %d pages and %d links: this gate examined nothing", pages, links)
	}
}

// TestDocsmdReadsLinksAsCommonMarkDoes pins the constructs the one-line
// pattern got wrong, and the code rules that decide what is a link at all.
// Each case has no reference definitions, so docsmd's inline targets and
// goldmark's destinations must agree exactly, both ways.
func TestDocsmdReadsLinksAsCommonMarkDoes(t *testing.T) {
	cases := map[string]string{
		"double-quoted title":       `See [the design](../../internal/design/x.md "Design record").`,
		"single-quoted title":       `See [a](../../internal/a.md 'T').`,
		"parenthesised title":       `See [a](../../internal/a.md (T)).`,
		"padded destination":        `See [a]( ../../internal/a.md ).`,
		"title on the next line":    "See [a](../../internal/a.md\n  \"T\").",
		"angle-bracket destination": `See [a](<../../internal/a b.md>) and [c](<../x.md> "T").`,
		"empty destination":         `See [a]() here.`,
		"image in the link text":    `[![badge](https://example.com/b.svg)](../../internal/x.md)`,
		"relative image in text":    `[![badge](assets/b.svg "B")](../x.md "X")`,
		"balanced brackets in text": `[see [1]](../x.md) and [a [b] c](../y.md)`,
		"link in a link's text":     `[outer [inner](../i.md)](../o.md)`,
		"link in an image's alt":    `![alt [inner](../i.md)](../o.png)`,
		"escaped brackets":          `\[not](../n.md) and [a\]b](../y.md)`,
		"text across lines":         "[the\ndesign](../../internal/x.md)",
		"no link across a blank":    "[the\n\ndesign](../../internal/x.md)",
		"space before the paren":    `[a] (../../internal/x.md)`,
		"space in a bare target":    `[a](../x y.md)`,
		"unterminated title":        `[a](../x.md "open)`,
		"parentheses in the target": `[a](../x_(y).md)`,
		"a quoted link":             "> See [the\n> design](\n> ../../internal/x.md).",
		"code span with a link":     "Code `[not](../n.md)` and [yes](../y.md).",
		"backticks in the target":   "[a](../x`y`.md) and `[b](../n.md)`",
		"code span across lines":    "Use `foo\n[x](../../internal/z.md)` here.",
		"closer after a line code":  "Use `foo\nbar` and [x](../y.md) and `baz`.",
		"code span ends at a blank": "Use `foo\n\n[x](../y.md)\n\nand ` here.",
		"list item interrupts":      "Use `a\n- item [x](../y.md) and ` b",
		"heading interrupts":        "Use `a\n# Head [x](../y.md) ` b",
		"backtick info is no fence": "```inline``` is a code span, not a fence.\n\nSee [the design](../../internal/x.md).",
		"tilde fence keeps ticks":   "~~~ `x`\n[a](../n.md)\n~~~\n[b](../y.md)",
		"fenced code":               "```\n[a](../n.md)\n```\n[b](../y.md)",
		"indented code":             "Para.\n\n    [a](../n.md)\n\nText [b](../y.md).",
		"indented list content":     "- item\n\n    [a](../y.md)",
		"indented continuation":     "Para\n    [a](../y.md)",
		"indented after a list":     "- item\n\nPara.\n\n    [a](../n.md)",
		"indented after a heading":  "# Title\n    [a](../n.md)\n\nText [b](../y.md).",
		"indented in a quote":       "> Use `foo\n> bar` and [x](../y.md) and `baz`.",
		"ordered item continues":    "text\n2. [x](../y.md) `a\nb` c",
		"comment across a blank":    "a <!-- x\n\nb --> [c](../y.md)",
		"escaped pipe in a cell":    "| h |\n|---|\n| `a \\| [x](../n.md) b` |",
		"comment":                   "A <!-- [a](../n.md) --> and [b](../y.md).",
		"html tag with a backtick":  "<span title=\"`\"> [x](../y.md) `code`",
		"autolink":                  "See <https://example.com/a]b> and [x](../y.md).",
		"table cells split a span":  "| h | h | h |\n|---|---|---|\n| `a | [x](../y.md) | b` |",
		"link in a table":           "| h |\n|---|\n| [x](../y.md \"T\") |",
	}
	for name, page := range cases {
		want := commonmarkDestinations([]byte(page))
		got := docsmdTargets(page, true)
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, "\n") != strings.Join(got, "\n") {
			t.Errorf("%s: docsmd reads %q, CommonMark renders %q\n  page: %q", name, got, want, page)
		}
	}
}

// TestDocsmdReadsDefinitionsCommonMarkResolves covers reference definitions:
// every reference link goldmark resolves must reach docsmd through the
// definition its destination comes from.
func TestDocsmdReadsDefinitionsCommonMarkResolves(t *testing.T) {
	for name, page := range map[string]string{
		"plain":                  "See [the design][d].\n\n[d]: ../../internal/x.md\n",
		"angle and title":        "See [d].\n\n[d]: <../../internal/a b.md> \"T\"\n",
		"target on the next one": "See [d].\n\n[d]:\n  ../../internal/x.md\n",
		"in a block quote":       "> See [d].\n>\n> [d]: ../../internal/x.md\n",
		"in a list item":         "- See [d].\n\n- [d]: ../../internal/x.md\n",
		"collapsed":              "See [d][].\n\n[d]: ../../internal/x.md 'T'\n",
	} {
		want := commonmarkDestinations([]byte(page))
		if len(want) == 0 {
			t.Errorf("%s: goldmark resolved no reference link; the case tests nothing", name)
		}
		if miss := missing(want, docsmdTargets(page, false)); len(miss) > 0 {
			t.Errorf("%s: docsmd missed %q\n  page: %q", name, miss, page)
		}
	}
}
