package main

import (
	"net/url"
	"path/filepath"
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

// pathToURI is the inverse of uriToPath: it turns a filesystem path into a
// file:// document URI. Used to point the editor at a definition target,
// which the workspace graph reports as a path relative to the server root.
func pathToURI(p string) string {
	p = filepath.ToSlash(p)
	// Windows drive paths (C:/foo) need the leading slash back: file:///C:/foo.
	if !strings.HasPrefix(p, "/") {
		if len(p) >= 2 && p[1] == ':' {
			p = "/" + p
		}
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}
