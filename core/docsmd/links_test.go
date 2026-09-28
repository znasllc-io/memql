package docsmd

import (
	"strings"
	"testing"
)

func TestLinksOffsetsSurviveCodeMasking(t *testing.T) {
	content := "Intro `[not](a.md)` then [one](../x/one.md#anchor) and\n" +
		"```\n[fenced](f.md)\n```\n" +
		"<!-- [commented](c.md) -->\n" +
		"![alt text](img.png) and [ext](https://example.com).\n" +
		"[ref]: ../defs/target.md\n"
	links := Links(content)
	var targets []string
	for _, l := range links {
		targets = append(targets, l.Target)
		if content[l.TargetStart:l.TargetEnd] != l.Target {
			t.Errorf("target offsets of %q point at %q", l.Target, content[l.TargetStart:l.TargetEnd])
		}
	}
	if got, want := strings.Join(targets, " "), "../x/one.md#anchor img.png https://example.com ../defs/target.md"; got != want {
		t.Fatalf("targets = %s\nwant      %s", got, want)
	}

	one := links[0]
	if one.Text != "one" || one.Line != 1 || content[one.Start:one.End] != "[one](../x/one.md#anchor)" {
		t.Errorf("first link = %+v", one)
	}
	if one.Path() != "../x/one.md" || one.Fragment() != "#anchor" {
		t.Errorf("Path/Fragment = %q, %q", one.Path(), one.Fragment())
	}
	img := links[1]
	if !img.Image || img.Text != "alt text" || content[img.Start:img.End] != "![alt text](img.png)" || img.Line != 6 {
		t.Errorf("image = %+v", img)
	}
	def := links[3]
	if !def.Definition || def.Text != "ref" || content[def.Start:def.End] != "[ref]: ../defs/target.md" {
		t.Errorf("definition = %+v", def)
	}
}

func TestLinksIgnoresFootnoteDefinitions(t *testing.T) {
	if links := Links("Text[^1].\n\n[^1]: See the guide.\n"); len(links) != 0 {
		t.Errorf("a footnote read as a link: %+v", links)
	}
}

func TestLinksTextMayHoldCode(t *testing.T) {
	content := "See [`a[0]` in code](target.md)."
	links := Links(content)
	if len(links) != 1 || links[0].Text != "`a[0]` in code" || links[0].Target != "target.md" {
		t.Fatalf("links = %+v", links)
	}
}

// targets returns the targets Links reads on content, in order.
func targets(content string) []string {
	var out []string
	for _, l := range Links(content) {
		out = append(out, l.Target)
	}
	return out
}

// Every way CommonMark writes an inline destination is read, and the target
// offsets bound the destination alone -- inside angle brackets, before a
// title -- so writing a route there keeps the link a link.
func TestLinksReadsEveryDestinationForm(t *testing.T) {
	cases := map[string]string{
		`[a](../x.md "Title")`:  "../x.md",
		`[a](../x.md 'Title')`:  "../x.md",
		`[a](../x.md (Title))`:  "../x.md",
		`[a]( ../x.md )`:        "../x.md",
		"[a](../x.md\n  \"T\")": "../x.md",
		"[a](\n../x.md)":        "../x.md",
		`[a](<../x y.md>)`:      "../x y.md",
		`[a](<../x.md> "T")`:    "../x.md",
		`[a](../x_(y).md)`:      "../x_(y).md",
		`[a]()`:                 "",
	}
	for content, want := range cases {
		links := Links(content)
		if len(links) != 1 {
			t.Errorf("%q: read %d links, want 1", content, len(links))
			continue
		}
		l := links[0]
		if l.Target != want || content[l.TargetStart:l.TargetEnd] != want {
			t.Errorf("%q: target %q at %q, want %q", content, l.Target, content[l.TargetStart:l.TargetEnd], want)
		}
		if l.Start != 0 || l.End != len(content) || l.Text != "a" {
			t.Errorf("%q: construct %q, text %q", content, content[l.Start:l.End], l.Text)
		}
	}
}

func TestLinksRefusesWhatCommonMarkRefuses(t *testing.T) {
	for _, content := range []string{
		`[a] (../x.md)`,         // a space between text and destination
		`[a](../x y.md)`,        // a bare destination cannot hold a space
		`[a](../x.md "open)`,    // an unterminated title
		`[a](<../x.md)`,         // an unclosed angle bracket
		`[a](../x(.md)`,         // unbalanced parentheses
		"[a\n\nb](../x.md)",     // link text cannot cross a blank line
		`\[a](../x.md)`,         // an escaped bracket is text
		"[a](../x.md\n\n\"T\")", // nor can a tail
	} {
		if got := targets(content); len(got) != 0 {
			t.Errorf("%q: read %q, which CommonMark renders as text", content, got)
		}
	}
}

// Link text holds balanced brackets and images; a link nested in a link's
// text makes the outer brackets text, as CommonMark reads it.
func TestLinksNesting(t *testing.T) {
	content := "[![badge](b.svg)](../x.md)"
	links := Links(content)
	if len(links) != 2 || links[0].Target != "../x.md" || links[1].Target != "b.svg" || !links[1].Image {
		t.Fatalf("links = %+v", links)
	}
	outer := links[0]
	if outer.Text != "![badge](b.svg)" || content[outer.Start:outer.TextStart] != "[" || content[outer.TextEnd:outer.End] != "](../x.md)" {
		t.Errorf("outer = %+v", outer)
	}
	for content, want := range map[string]string{
		"[see [1]](../x.md)":                "../x.md",
		"[a [b] c](../x.md)":                "../x.md",
		"[outer [inner](../i.md)](../o.md)": "../i.md",
		"![alt [inner](../i.md)](../o.png)": "../o.png ../i.md",
		"[a\\]b](../x.md)":                  "../x.md",
		"[a <b title=\"]\">](../x.md)":      "../x.md",
	} {
		if got := strings.Join(targets(content), " "); got != want {
			t.Errorf("%q: read %q, want %q", content, got, want)
		}
	}
}

func TestLinksAcrossLinesAndQuotes(t *testing.T) {
	for content, want := range map[string]string{
		"See [the\ndesign](../x.md).":         "../x.md",
		"> See [the\n> design](\n> ../x.md).": "../x.md",
		"- item [a\n  b](../x.md)":            "../x.md",
	} {
		if got := strings.Join(targets(content), " "); got != want {
			t.Errorf("%q: read %q, want %q", content, got, want)
		}
	}
}

func TestLinksReadsDefinitionForms(t *testing.T) {
	for content, want := range map[string]string{
		"[r]: ../x.md":               "../x.md",
		"[r]: <../x y.md> \"Title\"": "../x y.md",
		"[r]:\n  ../x.md":            "../x.md",
		"> [r]: ../x.md":             "../x.md",
		"- [r]: ../x.md":             "../x.md",
		"   [r]: ../x.md 'T'":        "../x.md",
		"[a\\]b]: ../x.md":           "../x.md",
		"[^1]: See ../x.md.":         "",
		"[r]:":                       "",
		"`[r]: ../x.md`":             "",
		"```\n[r]: ../x.md\n```":     "",
	} {
		if got := strings.Join(targets(content), " "); got != want {
			t.Errorf("%q: read %q, want %q", content, got, want)
		}
	}
	// A definition's construct runs to the end of its title, so dropping it
	// leaves no stray title behind.
	content := "[r]: ../x.md\n  \"Title\"\nNext."
	l := Links(content)[0]
	if !l.Definition || content[l.Start:l.End] != "[r]: ../x.md\n  \"Title\"" || l.Text != "r" {
		t.Errorf("definition = %+v (%q)", l, content[l.Start:l.End])
	}
}

func TestIsExternal(t *testing.T) {
	for target, want := range map[string]bool{
		"https://memql.io/docs/":   true,
		"http://example.com":       true,
		"mailto:someone@acme.test": true,
		"#same-page":               true,
		"//cdn.example.com/x.js":   true,
		"ftp://host/x":             true,
		"../internal/x.md":         false,
		"guide.md#part":            false,
		"/docs/internal/x.md":      false,
		"assets/diagram.svg":       false,
		"":                         true, // [text]() is the page itself
	} {
		if got := IsExternal(target); got != want {
			t.Errorf("IsExternal(%q) = %v, want %v", target, got, want)
		}
	}
}
