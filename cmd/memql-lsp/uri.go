package main

import (
	"net/url"
	"strings"
)

// uriToPath converts a file:// document URI to a filesystem path (best-effort).
// A non-file URI or a parse failure returns the raw string; Sense uses the path
// only for context, so an imperfect value never breaks analysis.
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p := u.Path
	// Windows drive paths arrive as /C:/foo -> C:/foo; harmless on POSIX.
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = strings.TrimPrefix(p, "/")
	}
	return p
}
