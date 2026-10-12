package emaildoc

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The sentences safeMarkup says, one per kind of problem.
const (
	wantComments   = "html blocks cannot contain comments"
	wantCutOff     = "html blocks cannot end inside a tag; close every tag with >"
	wantUnbalanced = "html blocks must close the tags they open, in order"
	wantPicture    = "html blocks cannot contain pictures; use an image block"
	wantStyle      = "html block styles cannot load resources"
	wantStyleTag   = "html block styles cannot contain merge tags"
	wantTooDeep    = "html blocks are nested too deeply"
)

func wantOnlyInTable(tag string) string {
	return "html blocks can use <" + tag + "> only inside a <table> the block opens"
}

// acceptedMarkup is markup safeMarkup accepts, and the form it hands the
// renderer. That form is the parser's: lower case names, attributes in double
// quotes and escaped, void elements as <br/>, the tbody every table gets,
// the first of two equal attributes, and a repair for what browsers repair.
// Attributes of formatting elements (a, b, font, ...) come out sorted.
var acceptedMarkup = []struct{ name, src, want string }{
	{"nothing", ``, ``},
	{"text is escaped", `plain text & more < > "q" 'r'`, `plain text &amp; more &lt; &gt; &#34;q&#34; &#39;r&#39;`},
	{"entities stay entities", `A &amp; B &lt;tag&gt; &copy;`, `A &amp; B &lt;tag&gt; ©`},
	{"a less-than before a space is text", `1 < 2 & 3 > 2`, `1 &lt; 2 &amp; 3 &gt; 2`},
	{"a trailing less-than is text", `a <`, `a &lt;`},
	{"escaped markup is text", `x &lt;script&gt;alert(1)&lt;/script&gt;`, `x &lt;script&gt;alert(1)&lt;/script&gt;`},
	{
		"an email table gains its tbody",
		`<table width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%; background:#fff"><tr><td align="center" style="padding:0 24px; font-family:Arial, sans-serif">A &amp; B<br/>C</td></tr></table>`,
		`<table width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%; background:#fff"><tbody><tr><td align="center" style="padding:0 24px; font-family:Arial, sans-serif">A &amp; B<br/>C</td></tr></tbody></table>`,
	},
	{
		"inline formatting and a link",
		`<p><strong>bold</strong> <em>em</em> <a href="https://example.com/a?b=1" target="_blank" rel="noopener">link</a></p>`,
		`<p><strong>bold</strong> <em>em</em> <a href="https://example.com/a?b=1" rel="noopener" target="_blank">link</a></p>`,
	},
	{
		"every link tag",
		`<a href="{{unsubscribeUrl}}">x</a><a href="{{preferencesUrl}}">x</a><a href="{{viewUrl}}">x</a><a href="{{confirmUrl}}">x</a>`,
		`<a href="{{unsubscribeUrl}}">x</a><a href="{{preferencesUrl}}">x</a><a href="{{viewUrl}}">x</a><a href="{{confirmUrl}}">x</a>`,
	},
	{
		"email, phone and empty links",
		`<a href="mailto:a@example.com">x</a><a href="tel:+15551234567">x</a><a href="">x</a>`,
		`<a href="mailto:a@example.com">x</a><a href="tel:+15551234567">x</a><a href="">x</a>`,
	},
	{"merge tags in a path and query", `<a href="https://example.com/{{fields.slug}}?e={{email}}">x</a>`, `<a href="https://example.com/{{fields.slug}}?e={{email}}">x</a>`},
	{"tags and attributes are lower cased", `<UL><LI CLASS="a">one</LI></UL>`, `<ul><li class="a">one</li></ul>`},
	{"the first of two equal attributes wins", `<a href="https://ok" href="javascript:alert(1)">x</a>`, `<a href="https://ok">x</a>`},
	{
		"lists, headings, rules and quotes",
		`<ul><li>one</li></ul><ol><li>two</li></ol><h1>h</h1><hr><blockquote>q</blockquote><pre>p</pre><code>c</code><center>c</center><font face="Arial" color="#333" size="2">f</font>`,
		`<ul><li>one</li></ul><ol><li>two</li></ol><h1>h</h1><hr/><blockquote>q</blockquote><pre>p</pre><code>c</code><center>c</center><font color="#333" face="Arial" size="2">f</font>`,
	},
	{
		"global attributes",
		`<div class="a" id="b" title="t" lang="en" dir="ltr" role="presentation" aria-label="l" aria-hidden="true">x</div>`,
		`<div class="a" id="b" title="t" lang="en" dir="ltr" role="presentation" aria-label="l" aria-hidden="true">x</div>`,
	},
	{
		"cell attributes",
		`<table><tr><td bgcolor="#fff" valign="top" colspan="2" rowspan="2" scope="col" width="10" height="10">x</td></tr></table>`,
		`<table><tbody><tr><td bgcolor="#fff" valign="top" colspan="2" rowspan="2" scope="col" width="10" height="10">x</td></tr></tbody></table>`,
	},
	{
		"nested tables",
		`<table><tr><td><table><tr><td>x</td></tr></table></td></tr></table>`,
		`<table><tbody><tr><td><table><tbody><tr><td>x</td></tr></tbody></table></td></tr></tbody></table>`,
	},
	{
		"every table part",
		`<table><caption>c</caption><thead><tr><th>h</th></tr></thead><tbody><tr><td>b</td></tr></tbody><tfoot><tr><td>f</td></tr></tfoot></table>`,
		`<table><caption>c</caption><thead><tr><th>h</th></tr></thead><tbody><tr><td>b</td></tr></tbody><tfoot><tr><td>f</td></tr></tfoot></table>`,
	},
	{
		"column groups",
		`<table><colgroup><col><col></colgroup><tr><td>x</td></tr></table>`,
		`<table><colgroup><col/><col/></colgroup><tbody><tr><td>x</td></tr></tbody></table>`,
	},
	{"a slash does not close a div", `<div/>x</div>`, `<div>x</div>`},
	{"a rule closes a paragraph", `<p>a<hr>b</p>`, `<p>a</p><hr/>b<p></p>`},
	{"a table closes a paragraph", `<p><table><tr><td>x</td></tr></table></p>`, `<p></p><table><tbody><tr><td>x</td></tr></tbody></table><p></p>`},
	{"a tag in a title is text", `<p title="Hi {{displayName}}">x</p>`, `<p title="Hi {{displayName}}">x</p>`},
	{"a style without a tag", `<p style="color:#333; width:100%">x</p>`, `<p style="color:#333; width:100%">x</p>`},
	{"a quote in an attribute is escaped", `<a title='x" onmouseover="alert(1)'>x</a>`, `<a title="x&#34; onmouseover=&#34;alert(1)">x</a>`},
	{"a line break is a void element", `a<br>b<br/>c`, `a<br/>b<br/>c`},
	{"a hundred levels deep", strings.Repeat("<div>", 100) + "x" + strings.Repeat("</div>", 100), strings.Repeat("<div>", 100) + "x" + strings.Repeat("</div>", 100)},
}

// refusedMarkup is markup safeMarkup refuses, and what it says.
var refusedMarkup = []struct{ name, src, want string }{
	// Every comment form: a comment is how markup is shown to one client and
	// hidden from the rest, and an unterminated one hides the rest of the email.
	{"a comment", `<!-- a plain comment -->`, wantComments},
	{"a bracketed comment", `<!-- [note] -->`, wantComments},
	{"an entity in a comment", `<!-- a &lt; b -->`, wantComments},
	{"a comment after markup", `<p>ok</p><!-- trailing -->`, wantComments},
	{"a conditional comment", `<!--[if mso]><v:rect/><![endif]-->`, wantComments},
	{"a conditional comment with markup", `<!--[if mso]>x<![endif]-->`, wantComments},
	{"a conditional comment with no end", `<!--[if mso]>x-->`, wantComments},
	{"a revealed conditional", `<![if !mso]><p>x</p><![endif]>`, wantComments},
	{"a bogus declaration", `<!x>`, wantComments},
	{"an xml declaration", `<?xml version="1.0"?>`, wantComments},
	{"a processing instruction", `<?php echo 1 ?>`, wantComments},
	{"a bogus end tag", `</3 foo>`, wantComments},
	{"cdata", `<![CDATA[x]]>`, wantComments},
	{"a doctype", `<!DOCTYPE html>`, wantComments},
	{"a doctype cut off", `<!DOCTYPE html`, wantComments},
	{"an unterminated comment", `<!-- unterminated`, wantComments},
	{"a comment opener", `<!--`, wantComments},
	{"a short comment", `<!-->`, wantComments},
	{"another short comment", `<!--->`, wantComments},
	{"an empty comment", `<!---->`, wantComments},
	{"half a comment opener", `<!-`, wantComments},
	{"a declaration opener", `<!`, wantComments},
	// A tag the fragment ends inside is dropped by the tokenizer, so a check
	// that looks at tokens never sees it; spliced into a cell it still runs.
	{"a picture cut off", `<img src="https://evil.example/p.gif"`, wantCutOff},
	{"a link cut off", `<a href="javascript:alert(1)"`, wantCutOff},
	{"a script cut off", `<script src=https://evil.example/x.js`, wantCutOff},
	{"a handler cut off", `<p onclick="alert(1)"`, wantCutOff},
	{"a tag name alone", `<p`, wantCutOff},
	{"a tag name and a space", `<p `, wantCutOff},
	{"text then a tag", `x <b`, wantCutOff},
	{"an end tag cut off", `</p`, wantCutOff},
	{"after complete markup", `<p>ok</p><b`, wantCutOff},
	{"a quote left open", `<div class="x`, wantCutOff},
	{"a single quote left open", `<a href='https://x`, wantCutOff},
	{"an attribute without a value", `<p hidden`, wantCutOff},
	{"cut off wins over left open", `<p>one <b`, wantCutOff},
	// Closing what the fragment itself opened, in order: anything else closes
	// the renderer's own cell or row.
	{"a stray cell end", `</td>`, wantUnbalanced},
	{"closing the renderer's cell", `</td></tr></table><p>x</p>`, wantUnbalanced},
	{"closing the renderer's table", `</table>`, wantUnbalanced},
	{"a stray paragraph end", `</p>`, wantUnbalanced},
	{"a stray line break end", `</br>`, wantUnbalanced},
	{"an end tag after text", `x</div>`, wantUnbalanced},
	{"a paragraph left open", `<p>one`, wantUnbalanced},
	{"an inline element left open", `<b>x`, wantUnbalanced},
	{"closed out of order", `<p><b>x</p></b>`, wantUnbalanced},
	{"divs closed out of order", `<div><p>x</div></p>`, wantUnbalanced},
	{"a table left open", `<table><tr><td>x</td></tr>`, wantUnbalanced},
	{"a slash opens a div", `<div/>x`, wantUnbalanced},
	{"one end tag too many", `<p>a</p></p>`, wantUnbalanced},
	{"a list left open", `<ul><li>a</li>`, wantUnbalanced},
	{"nothing closes a rule", `<hr></hr>`, wantUnbalanced},
	// Table parts only inside a table the fragment opened: a cell inside the
	// renderer's cell closes it.
	{"a bare cell", `<td>x</td>`, wantOnlyInTable("td")},
	{"a bare row", `<tr><td>x</td></tr>`, wantOnlyInTable("tr")},
	{"a bare header cell", `<th>x</th>`, wantOnlyInTable("th")},
	{"a bare head", `<thead></thead>`, wantOnlyInTable("thead")},
	{"a bare body", `<tbody></tbody>`, wantOnlyInTable("tbody")},
	{"a bare foot", `<tfoot></tfoot>`, wantOnlyInTable("tfoot")},
	{"a bare caption", `<caption>c</caption>`, wantOnlyInTable("caption")},
	{"a bare column group", `<colgroup></colgroup>`, wantOnlyInTable("colgroup")},
	{"a bare column", `<col>`, wantOnlyInTable("col")},
	{"a cell in a paragraph", `<p><td>x</td></p>`, wantOnlyInTable("td")},
	{"a row in a div", `<div><tr><td>x</td></tr></div>`, wantOnlyInTable("tr")},
	{"a cell after a table", `<table><tr><td>x</td></tr></table><td>y</td>`, wantOnlyInTable("td")},
	// Entity and whitespace variants of a script link.
	{"an entity-encoded tab in a scheme", `<a href="java&#x09;script:x">x</a>`, "a link cannot contain spaces or control characters"},
	{"a space before a scheme", `<a href=" javascript:x">x</a>`, "a link cannot contain spaces or control characters"},
	{"a line feed in a scheme", "<a href=\"java\nscript:x\">x</a>", "a link cannot contain spaces or control characters"},
	{"an entity-encoded letter in a scheme", `<a href="jav&#x61;script:x">x</a>`, "a link must be"},
	{"a script link", `<a href="javascript:alert(1)">x</a>`, "a link must be"},
	{"a mixed case script link", `<a href="JavaScript:alert(1)">x</a>`, "a link must be"},
	{"a script link after a good one", `<a href="javascript:alert(1)" href="https://ok">x</a>`, "a link must be"},
	{"a field tag link", `<a href="{{fields.url}}">x</a>`, "a link must be"},
	{"a fragment link", `<a href="#top">x</a>`, "a link must be"},
	{"a relative link", `<a href="/relative">x</a>`, "a link must be"},
	{"a data link", `<a href="data:text/html;base64,AAAA">x</a>`, "a link must be"},
	{"a tag in the host", `<a href="https://{{fields.host}}/x">x</a>`, "a web address cannot take a merge tag in its host, user name or port"},
	{"a quote in a link", `<a href="https://example.com/a&quot;b">x</a>`, "a link cannot contain quotes, angle brackets, backticks or backslashes"},
	// What was never allowed.
	{"a picture", `<img src="x">`, wantPicture},
	{"a picture in capitals", `<IMG SRC="x">`, wantPicture},
	{"a picture without a source", `<img>`, wantPicture},
	{"a picture end tag", `</img>`, wantPicture},
	{"a script", `<script>1</script>`, "html blocks cannot use <script>"},
	{"a script in capitals", `<SCRIPT>1</SCRIPT>`, "html blocks cannot use <script>"},
	{"a script end tag", `</script>`, "html blocks cannot use <script>"},
	{"a style element", `<style>p{}</style>`, "html blocks cannot use <style>"},
	{"an svg", `<svg><image href="https://x"/></svg>`, "html blocks cannot use <svg>"},
	{"an iframe", `<iframe src="x"></iframe>`, "html blocks cannot use <iframe>"},
	{"a form", `<form><input name="a"></form>`, "html blocks cannot use <form>"},
	{"a stylesheet link", `<link rel="stylesheet" href="x">`, "html blocks cannot use <link>"},
	{"a refresh", `<meta http-equiv="refresh" content="0">`, "html blocks cannot use <meta>"},
	{"a base", `<base href="https://x">`, "html blocks cannot use <base>"},
	{"an object", `<object data="x"></object>`, "html blocks cannot use <object>"},
	{"a video", `<video src="x"></video>`, "html blocks cannot use <video>"},
	{"a picture element", `<picture><source srcset="x"></picture>`, "html blocks cannot use <picture>"},
	{"a document", `<html><body>x</body></html>`, "html blocks cannot use <html>"},
	{"a head", `<head></head>`, "html blocks cannot use <head>"},
	{"a title", `<title>x</title>`, "html blocks cannot use <title>"},
	{"a vml shape", `<v:rect/>`, "html blocks cannot use <v:rect>"},
	{"a click handler", `<p onclick="x">a</p>`, "html blocks cannot use the onclick attribute"},
	{"a hover handler", `<p onmouseover="x">a</p>`, "html blocks cannot use the onmouseover attribute"},
	{"a data attribute", `<p data-x="1">a</p>`, "html blocks cannot use the data-x attribute"},
	{"a ping", `<a href="https://x" ping="https://y">x</a>`, "html blocks cannot use the ping attribute"},
	{"a table background", `<table background="https://x"><tr><td>x</td></tr></table>`, "html blocks cannot use the background attribute"},
	{"a source attribute", `<div src="x">x</div>`, "html blocks cannot use the src attribute"},
	{"a source set", `<div srcset="x">x</div>`, "html blocks cannot use the srcset attribute"},
	{"an href on a div", `<div href="https://x">x</div>`, "html blocks cannot use the href attribute"},
	{"a style url", `<p style="background:url(https://x)">x</p>`, wantStyle},
	{"a style URL in capitals", `<p style="background:URL(https://x)">x</p>`, wantStyle},
	{"a style url hidden by an escape", `<p style="background:u\72l(https://x)">x</p>`, wantStyle},
	{"a style url hidden by a comment", `<p style="background:u/**/rl(https://x)">x</p>`, wantStyle},
	{"a style url hidden by an entity", `<p style="background:u&#x72;l(https://x)">x</p>`, wantStyle},
	{"a style image", `<p style="background:image('https://x')">x</p>`, wantStyle},
	{"a style image set", `<p style="background:image-set('x' 1x)">x</p>`, wantStyle},
	{"a style expression", `<p style="width:expression(alert(1))">x</p>`, wantStyle},
	{"a style behavior", `<p style="behavior:url(x)">x</p>`, wantStyle},
	{"a style binding", `<p style="-moz-binding:x">x</p>`, wantStyle},
	{"a style import", `<p style="@import 'x'">x</p>`, wantStyle},
	// A merge tag is replaced with a value a recipient supplied; in a style that
	// value would be CSS, and the checks above cannot see what it becomes.
	{"a tag alone in a style", `<p style="color:{{fields.c}}">x</p>`, wantStyleTag},
	{"a tag after a valid declaration", `<p style="color:#333; width:{{fields.w}}">x</p>`, wantStyleTag},
	{"a tag that is a whole declaration", `<p style="{{fields.css}}">x</p>`, wantStyleTag},
	{"a tag in a length", `<p style="padding:{{fields.p}}px">x</p>`, wantStyleTag},
	{"a tag that could spell url", `<p style="background:u{{fields.a}}rl(https://example.com/a.png)">x</p>`, wantStyleTag},
	{"a tag in the middle of url", `<p style="background:ur{{fields.a}}(https://example.com/a.png)">x</p>`, wantStyleTag},
	{"a tag inside a url call", `<p style="background:url({{fields.u}})">x</p>`, wantStyleTag},
	{"a name tag in a style", `<p style="color:{{displayName}}">x</p>`, wantStyleTag},
	{"a link tag in a style", `<p style="background:{{viewUrl}}">x</p>`, wantStyleTag},
	{"a tag written with entities", `<p style="color:&#123;&#123;fields.c&#125;&#125;">x</p>`, wantStyleTag},
	{"a tag in a style attribute in capitals", `<p STYLE="color:{{fields.c}}">x</p>`, wantStyleTag},
	{"a tag in the style of a cell", `<table><tr><td style="width:{{fields.w}}">x</td></tr></table>`, wantStyleTag},
	{"a tag in the style of a link", `<a href="https://example.com" style="color:{{fields.c}}">x</a>`, wantStyleTag},
	{"nested a hundred and one deep", strings.Repeat("<div>", 101) + strings.Repeat("</div>", 101), wantTooDeep},
	{"nested far too deep", strings.Repeat("<div>", 5000), wantTooDeep},
}

func TestSafeMarkupAccepts(t *testing.T) {
	for _, c := range acceptedMarkup {
		t.Run(c.name, func(t *testing.T) {
			got, err := safeMarkup(c.src)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("\n got: %s\nwant: %s", got, c.want)
			}
			if problem := leavesItsCell(got); problem != "" {
				t.Fatal(problem)
			}
		})
	}
}

func TestSafeMarkupRefuses(t *testing.T) {
	for _, c := range refusedMarkup {
		t.Run(c.name, func(t *testing.T) {
			if out, err := safeMarkup(c.src); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %q and %v", c.want, out, err)
			}
		})
	}
}

// The renderer puts a block's markup inside a table cell. These are written to
// close it and carry on, to open a second cell, or to run.
var breakouts = []string{
	`</td></tr></tbody></table><b>evil</b>`,
	`</td></tr></tbody></table><table><tr><td>`,
	`</td><td>second</td><td>`,
	`</tr><tr><td>`,
	`<td>second cell</td>`,
	`<tr><td>second row</td></tr>`,
	`</table><p>after the table</p>`,
	`<img src="https://evil.example/p.gif"`,
	`<a href="javascript:alert(1)"`,
	`<p onclick="alert(1)"`,
	`<!-- unterminated`,
	`<p>one<p>two`,
	`<table><div>stray</div><tr><td>y</td></tr></table>`,
	`<table><tr><td></td></tr></table></td></tr></table>`,
	`<b><p>x</b>y</p>`,
	`<a><table><tr><td>x</td></tr></table></a>`,
}

func TestMarkupStaysInsideItsCell(t *testing.T) {
	// The harness first: markup written to leave the cell is seen to leave it,
	// so passing means something.
	for _, escape := range breakouts[:7] {
		if leavesItsCell(escape) == "" {
			t.Errorf("the harness did not notice that %q leaves the cell", escape)
		}
	}
	if problem := leavesItsCell(`hello <b>world</b> <table><tbody><tr><td>x</td></tr></tbody></table>`); problem != "" {
		t.Errorf("the harness refused ordinary markup: %s", problem)
	}
	// Then the claim: whatever safeMarkup accepts, in the form it hands out,
	// stays in the cell. The rest it refuses.
	for _, attack := range breakouts {
		out, err := safeMarkup(attack)
		if err != nil {
			continue
		}
		if problem := leavesItsCell(out); problem != "" {
			t.Errorf("%q becomes %q, which leaves the cell: %s", attack, out, problem)
		}
	}
}

func TestSafeMarkupChecksWhatTheParserBuilds(t *testing.T) {
	// The tokens are checked first, so nothing the parser builds should differ
	// from them; the second look is for the day it does. These trees cannot be
	// reached through safeMarkup, so they are checked directly.
	element := func(name string, attrs ...html.Attribute) *html.Node {
		return &html.Node{Type: html.ElementNode, Data: name, Attr: attrs}
	}
	nested := element("div")
	nested.AppendChild(element("p"))
	nested.FirstChild.AppendChild(element("script"))
	for _, c := range []struct {
		name string
		node *html.Node
		want string
	}{
		{"a script", element("script"), "html blocks cannot use <script>"},
		{"a picture", element("img"), wantPicture},
		{"a handler", element("p", html.Attribute{Key: "onclick", Val: "x"}), "html blocks cannot use the onclick attribute"},
		{"a loading style", element("p", html.Attribute{Key: "style", Val: "background:url(x)"}), wantStyle},
		{"a style with a merge tag", element("p", html.Attribute{Key: "style", Val: "color:{{fields.c}}"}), wantStyleTag},
		{"a script link", element("a", html.Attribute{Key: "href", Val: "javascript:x"}), "a link must be"},
		{"a host tag", element("a", html.Attribute{Key: "href", Val: "https://{{fields.host}}/"}), "a web address cannot take a merge tag"},
		{"a namespaced attribute", element("a", html.Attribute{Namespace: "xlink", Key: "href", Val: "https://example.com"}), "html blocks cannot use the href attribute"},
		{"a foreign element", &html.Node{Type: html.ElementNode, Namespace: "svg", Data: "a"}, "html blocks cannot use <a>"},
		{"a comment", &html.Node{Type: html.CommentNode, Data: "x"}, wantComments},
		{"a doctype", &html.Node{Type: html.DoctypeNode, Data: "html"}, wantComments},
		{"a deep script", nested, "html blocks cannot use <script>"},
	} {
		if err := checkNodes([]*html.Node{c.node}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
	fine := element("p", html.Attribute{Key: "style", Val: "color:#333"})
	fine.AppendChild(&html.Node{Type: html.TextNode, Data: "<b>"})
	if err := checkNodes([]*html.Node{fine, {Type: html.TextNode, Data: "x"}}); err != nil {
		t.Errorf("ordinary nodes: %v", err)
	}
}

func TestSafeMarkupDoesNotTrustTheParser(t *testing.T) {
	// The token checks run first, so for now nothing the parser builds from
	// markup that passed them is anything but allowed. Should that change, or
	// the parser fail, the markup is refused: a parser that misbehaves is made
	// to, here.
	real := parseFragment
	t.Cleanup(func() { parseFragment = real })
	for _, c := range []struct {
		name  string
		parse func(io.Reader, *html.Node) ([]*html.Node, error)
		want  string
	}{
		{"builds a script", func(io.Reader, *html.Node) ([]*html.Node, error) {
			return []*html.Node{{Type: html.ElementNode, Data: "script"}}, nil
		}, "html blocks cannot use <script>"},
		{"builds a handler", func(io.Reader, *html.Node) ([]*html.Node, error) {
			return []*html.Node{{Type: html.ElementNode, Data: "p", Attr: []html.Attribute{{Key: "onclick", Val: "x"}}}}, nil
		}, "html blocks cannot use the onclick attribute"},
		{"builds a comment", func(io.Reader, *html.Node) ([]*html.Node, error) {
			return []*html.Node{{Type: html.CommentNode, Data: "x"}}, nil
		}, wantComments},
		{"fails", func(io.Reader, *html.Node) ([]*html.Node, error) {
			return nil, errors.New("html: open stack of elements exceeds 512 nodes")
		}, wantTooDeep},
	} {
		parseFragment = c.parse
		if out, err := safeMarkup("x"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("a parser that %s: want %q, got %q and %v", c.name, c.want, out, err)
		}
	}
}

// markupPieces are what TestSafeMarkupRandomFragments strings together: tags
// open and shut, table parts, entities, stray angle brackets and quotes, and
// the slash on a tag that is not a void element.
var markupPieces = []string{
	"<p>", "</p>", "<b>", "</b>", "<i>", "</i>", `<a href="https://example.com/x">`, `<a href="{{viewUrl}}">`, "</a>",
	"<table>", "</table>", "<tr>", "</tr>", "<td>", "</td>", "<th>", "</th>", "<tbody>", "</tbody>", "<thead>", "</thead>",
	"<caption>", "</caption>", "<colgroup>", "</colgroup>", "<col>", "<div>", "</div>", `<span style="color:#333">`, "</span>",
	"<ul>", "</ul>", "<ol>", "</ol>", "<li>", "</li>", "<br>", "<br/>", "<hr>", "<hr/>", `<font color="#333" face="Arial">`, "</font>",
	"<pre>", "</pre>", "<code>", "</code>", "<h1>", "</h1>", "<center>", "</center>", "<blockquote>", "</blockquote>",
	"<strong>", "</strong>", "<em>", "</em>", "<u>", "</u>", "<s>", "</s>", "<small>", "</small>", "<sup>", "</sup>", "<sub>", "</sub>",
	"text", " ", "\n", "&amp;", "&lt;", "&gt;", "&#x3c;", "&quot;", "a < b", "x > y", `"`, "'", "<", ">", "&", "{{displayName}}",
	"<div/>", "<td/>", "<p/>", `<a href="https://example.com" href="javascript:1">`, `<td colspan="2">`, "<tr><td>", "</td></tr>",
}

func TestSafeMarkupRandomFragments(t *testing.T) {
	// Fragments strung together from the pieces above, most of them malformed.
	// Whatever safeMarkup lets through must stay in its cell; the rest it
	// refuses. The run is seeded, so a failure repeats.
	rng := rand.New(rand.NewSource(7))
	accepted := 0
	for attempts := 0; accepted < 3000 && attempts < 2_000_000; attempts++ {
		var b strings.Builder
		for i, n := 0, 1+rng.Intn(14); i < n; i++ {
			b.WriteString(markupPieces[rng.Intn(len(markupPieces))])
		}
		src := b.String()
		out, err := safeMarkup(src)
		if err != nil {
			continue
		}
		accepted++
		if problem := leavesItsCell(out); problem != "" {
			t.Fatalf("%q becomes %q, which leaves the cell: %s", src, out, problem)
		}
	}
	if accepted < 3000 {
		t.Fatalf("only %d fragments were accepted: the pieces no longer make well-formed markup often enough to test anything", accepted)
	}
}

// leavesItsCell splices markup into a table cell the way the renderer does,
// reads the page the way a browser does, and says how the page differs from one
// table with one cell followed by one paragraph; "" when it does not differ.
// The markers are data- attributes, which an html block cannot carry.
func leavesItsCell(markup string) string {
	page := `<!DOCTYPE html><html><head></head><body>` +
		`<table data-wrapper="outer"><tbody><tr><td data-wrapper="cell">` + markup +
		`</td></tr></tbody></table><p data-wrapper="after">after</p></body></html>`
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "the page does not parse: " + err.Error()
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	// Elements, and text that is not blank; comments count as well.
	significant := func(n *html.Node) []*html.Node {
		var kids []*html.Node
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.TextNode || strings.TrimSpace(c.Data) != "" {
				kids = append(kids, c)
			}
		}
		return kids
	}
	var body *html.Node
	wrappers := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "body" && body == nil {
				body = n
			}
			if attr(n, "data-wrapper") != "" {
				wrappers++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if body == nil || wrappers != 3 {
		return fmt.Sprintf("the page has %d of its 3 marked elements", wrappers)
	}
	top := significant(body)
	if len(top) != 2 || top[0].Type != html.ElementNode || top[1].Type != html.ElementNode ||
		attr(top[0], "data-wrapper") != "outer" || attr(top[1], "data-wrapper") != "after" {
		return "the page body holds more than the table and the closing paragraph"
	}
	inner := top[0]
	for _, name := range []string{"tbody", "tr", "td"} {
		kids := significant(inner)
		if len(kids) != 1 || kids[0].Type != html.ElementNode || kids[0].Data != name {
			return "the table no longer holds exactly one " + name
		}
		inner = kids[0]
	}
	if attr(inner, "data-wrapper") != "cell" {
		return "the cell is not the renderer's"
	}
	if after := significant(top[1]); len(after) != 1 || after[0].Type != html.TextNode || after[0].Data != "after" {
		return "the closing paragraph changed"
	}
	return ""
}

func FuzzSafeMarkup(f *testing.F) {
	for _, c := range acceptedMarkup {
		f.Add(c.src)
	}
	for _, c := range refusedMarkup {
		f.Add(c.src)
	}
	for _, attack := range breakouts {
		f.Add(attack)
	}
	f.Fuzz(func(t *testing.T, src string) {
		out, err := safeMarkup(src)
		if err != nil {
			return
		}
		if problem := leavesItsCell(out); problem != "" {
			t.Fatalf("%q becomes %q, which leaves the cell: %s", src, out, problem)
		}
	})
}
