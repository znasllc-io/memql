package visit

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxPathBytes = 256

// IsPageView says whether a request is a person opening a page, as opposed to
// a script, stylesheet, image or a browser warming a cache.
//
// With Fetch Metadata headers the browser states the answer itself (a
// top-level navigation to a document) and that is trusted over guessing from
// the path. Prefetch and prerender are refused: the page was not shown yet.
// Older clients send no Sec-Fetch-* headers; for them a GET that accepts HTML
// on a document or fallback path is the best available evidence. It does not
// look at the user agent; the caller excludes bots.
func IsPageView(r *http.Request, pathClass string, status int) bool {
	if r.Method != http.MethodGet || (status != http.StatusOK && status != http.StatusNotModified) {
		return false
	}
	for _, h := range []string{"Sec-Purpose", "Purpose"} {
		v := strings.ToLower(r.Header.Get(h))
		if strings.Contains(v, "prefetch") || strings.Contains(v, "prerender") {
			return false
		}
	}
	mode, dest := r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest")
	if mode != "" || dest != "" {
		return mode == "navigate" && dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html") &&
		(pathClass == "document" || pathClass == "fallback")
}

// NormalizePath is the path a statistic groups on: clean text (cleanText), no
// query or fragment (they carry tokens and would split one page into
// thousands), no trailing slash, and bounded so a hostile URL cannot grow a
// row. "" means not a path.
//
// Cleaned FIRST, so every later rule judges the path as it will be stored: a
// trailing slash hidden behind a control character is still a trailing slash.
func NormalizePath(p string) string {
	p = cleanText(p)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return truncate(p, maxPathBytes)
}

// cleanText keeps only text that can be stored and read back as written:
// valid UTF-8 with no control characters (below U+0020, and DEL).
//
// THE INPUTS ARE THE CLIENT'S TO WRITE. A request path and a query value
// arrive percent-decoded, so "%00" is a NUL and "%ff" a byte that is not
// UTF-8, and a ping's body is raw bytes. A NUL is the dangerous one: Postgres
// text cannot hold it, and the query builder fails the statement rather than
// strip it, so one such value fails the whole batch insert its row rides in
// and loses every other site's requests in it. Bytes that are not UTF-8 and
// the other control characters are dropped as well: they would be stored as
// replacement characters or invisible noise, and a value that differs only by
// an invisible byte splits one page into many.
//
// Dropped rather than replaced with U+FFFD, which would store a character the
// page's real path does not contain. Applied before any truncation, so a cut
// never has to reason about bytes that are not text.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// truncate cuts to at most n bytes. A cut that lands inside a multi-byte rune
// drops that rune's leading bytes and nothing more: it backs up over at most
// utf8.UTFMax-1 continuation bytes to the rune's start. Callers pass clean text
// (cleanText), so the cut is the only place a partial rune can come from.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	return s[:cut]
}
