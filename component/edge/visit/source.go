package visit

import (
	"net/http"
	"net/url"
	"strings"
)

// referrerFolds maps hosts that are one source under several names. Closed on
// purpose: a guess that folded an unknown host would hide a real source.
var referrerFolds = map[string]string{
	"l.facebook.com": "facebook.com",
	"m.facebook.com": "facebook.com",
	"t.co":           "x.com",
}

// The longest source stored from each origin. Both are written by whoever
// sent the request, so both are capped where they are derived.
const (
	maxUTMSourceBytes = 64
	// maxHostBytes is the longest a DNS name can be. url.Parse puts no limit
	// on a host, so without this one request could store a header-sized
	// string as its source.
	maxHostBytes = 253
)

// Source is where a visitor came from: an explicit utm_source wins (the
// sender said so), else the referring host. "" means an internal navigation,
// which is not a source; "direct" means nothing said.
//
// Both inputs are cleaned (cleanText) before anything is judged: a utm_source
// arrives percent-decoded, and a Referer host may carry bytes that are not
// UTF-8. A value with nothing left after cleaning says nothing.
func Source(r *http.Request, ownHosts ...string) string {
	if u := strings.ToLower(strings.TrimSpace(cleanText(r.URL.Query().Get("utm_source")))); u != "" {
		return truncate(u, maxUTMSourceBytes)
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return "direct"
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return "direct"
	}
	host := strings.TrimPrefix(strings.ToLower(cleanText(parsed.Hostname())), "www.")
	if host == "" {
		return "direct"
	}
	if isOwn(host, append([]string{r.Host}, ownHosts...)) {
		return ""
	}
	if f, ok := referrerFolds[host]; ok {
		return f
	}
	if isGoogle(host) {
		return "google"
	}
	return truncate(host, maxHostBytes)
}

// isGoogle accepts google.<tld> including two-label ones (co.uk, com.au) but
// not google.<anything>.<domain>, which anyone can register.
func isGoogle(host string) bool {
	rest, ok := strings.CutPrefix(host, "google.")
	if !ok {
		return false
	}
	labels := strings.Split(rest, ".")
	if len(labels) > 2 {
		return false
	}
	for _, l := range labels {
		if len(l) < 2 || len(l) > 3 {
			return false
		}
	}
	return true
}

func isOwn(host string, own []string) bool {
	for _, h := range own {
		h = strings.TrimPrefix(strings.ToLower(h), "www.")
		if hostOnly, _, err := splitHostPort(h); err == nil {
			h = hostOnly
		}
		if h != "" && h == host {
			return true
		}
	}
	return false
}

func splitHostPort(h string) (string, string, error) {
	u, err := url.Parse("//" + h)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}
