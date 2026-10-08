// Package readabletext extracts bounded-source web prose for retrieval adapters.
package readabletext

import (
	"regexp"
	"strings"
)

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|head)[^>]*>.*?</(script|style|noscript|head)>`)
	tagRe         = regexp.MustCompile(`(?s)<[^>]+>`)
	wsRe          = regexp.MustCompile(`[ \t]+`)
	blankLinesRe  = regexp.MustCompile(`\n{3,}`)
)

// Extract reduces an HTML (or plain-text) body to readable
// text. Deliberately simple -- strips script/style/head + tags, collapses
// whitespace. Good enough to feed the Trainer's reasoning model, which
// distills rather than parses structure. Plain-text bodies pass through
// untouched (no tags to strip).
func Extract(body, contentType string) string {
	if strings.Contains(strings.ToLower(contentType), "text/plain") {
		return strings.TrimSpace(body)
	}
	out := scriptStyleRe.ReplaceAllString(body, " ")
	out = tagRe.ReplaceAllString(out, " ")
	out = htmlUnescapeBasic(out)
	out = wsRe.ReplaceAllString(out, " ")
	// Normalize line breaks: collapse runs of blank lines.
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = blankLinesRe.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// htmlUnescapeBasic handles the handful of entities common in extracted
// prose. Avoids pulling in golang.org/x/net/html for a stub-level path.
func htmlUnescapeBasic(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&nbsp;", " ",
	)
	return r.Replace(s)
}
