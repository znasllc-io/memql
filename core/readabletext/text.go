// Package readabletext extracts bounded-source web prose for retrieval adapters.
package readabletext

import (
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// Extract keeps article prose and source links, without page navigation or
// executable markup. Callers bound the input and the returned excerpt. It is a
// text projection, not a sanitizer: retrieved content remains untrusted.
// Non-HTML responses retain their structure, including JSON and plain text.
func Extract(body, contentType string) string {
	typ := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if typ != "" && typ != "text/html" && typ != "application/xhtml+xml" {
		return strings.TrimSpace(body)
	}
	root, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return strings.TrimSpace(body)
	}
	// Prefer a declared main region. Only select an article on its own when
	// there is exactly one; a results page may have many article cards.
	var mains, articles []*html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if omitted(n) {
			return
		}
		if n.Type == html.ElementNode {
			if n.Data == "main" || attribute(n, "role") == "main" {
				mains = append(mains, n)
				return
			}
			if n.Data == "article" {
				articles = append(articles, n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(root)
	roots := []*html.Node{root}
	if len(mains) > 0 {
		roots = mains
	} else if len(articles) == 1 {
		roots = articles
	}
	var out textWriter
	for _, n := range roots {
		out.node(n, false)
		out.line()
	}
	return strings.TrimSpace(out.String())
}

func attribute(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func omitted(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for _, a := range n.Attr {
		if a.Key == "hidden" || (a.Key == "aria-hidden" && strings.EqualFold(a.Val, "true")) {
			return true
		}
	}
	switch n.Data {
	case "head", "script", "style", "noscript", "template", "nav", "svg", "canvas":
		return true
	}
	switch attribute(n, "role") {
	case "navigation", "banner", "contentinfo", "menu", "menubar", "toolbar":
		return true
	}
	// MediaWiki places its language and page-action menus inside the article's
	// main/header, without navigation roles. Recognize its exact menu class so
	// those links cannot consume the excerpt before any article prose arrives.
	// Do not discard the surrounding header: it also holds the article title.
	for _, class := range strings.Fields(attribute(n, "class")) {
		if class == "mw-portlet" {
			return true
		}
	}
	// An article's header/footer can contain its title, byline and references.
	if n.Data == "header" || n.Data == "footer" {
		for p := n.Parent; p != nil; p = p.Parent {
			if p.Data == "article" || p.Data == "main" || attribute(p, "role") == "main" {
				return false
			}
		}
		return true
	}
	return false
}

type textWriter struct {
	strings.Builder
	last rune
}

func (w *textWriter) put(r rune) {
	w.WriteRune(r)
	w.last = r
}

func (w *textWriter) line() {
	if w.Len() > 0 && w.last != '\n' {
		w.put('\n')
	}
}

func (w *textWriter) text(s string, pre bool) {
	for _, r := range s {
		if !pre && unicode.IsSpace(r) {
			if w.Len() > 0 && !unicode.IsSpace(w.last) {
				w.put(' ')
			}
			continue
		}
		w.put(r)
	}
}

func (w *textWriter) node(n *html.Node, pre bool) {
	if omitted(n) {
		return
	}
	if n.Type == html.TextNode {
		w.text(n.Data, pre)
		return
	}
	block := false
	if n.Type == html.ElementNode {
		switch n.Data {
		case "address", "article", "blockquote", "br", "dd", "div", "dl", "dt", "figcaption", "figure", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "li", "main", "ol", "p", "pre", "section", "table", "tr", "ul":
			block = true
			w.line()
		}
		pre = pre || n.Data == "pre"
		if n.Data == "img" {
			w.text(attribute(n, "alt"), pre)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.node(c, pre)
	}
	if n.Type == html.ElementNode {
		if n.Data == "a" {
			href := strings.TrimSpace(attribute(n, "href"))
			link, err := url.Parse(href)
			if err == nil && href != "" && !strings.HasPrefix(href, "#") &&
				(link.Scheme == "" || link.Scheme == "http" || link.Scheme == "https") {
				w.text(" ("+href+")", false)
			}
		}
		if n.Data == "td" || n.Data == "th" {
			w.text(" | ", false)
		}
	}
	if block {
		w.line()
	}
}
