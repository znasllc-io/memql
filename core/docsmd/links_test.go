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
	} {
		if got := IsExternal(target); got != want {
			t.Errorf("IsExternal(%q) = %v, want %v", target, got, want)
		}
	}
}
