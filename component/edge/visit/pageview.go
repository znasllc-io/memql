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

// NormalizePath is the path a statistic groups on: no query or fragment (they
// carry tokens and would split one page into thousands), no trailing slash,
// and bounded so a hostile URL cannot grow a row. "" means not a path.
func NormalizePath(p string) string {
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

// truncate cuts to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
