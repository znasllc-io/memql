package parser

import (
	"regexp"
	"strings"
)

// namespace_default_migrate.go -- the memql#2614 strip: with absent
// @namespace deriving the containing domain directory (both assemblers, in
// lockstep), a standalone `@namespace("<dir>")` whose value EQUALS the
// directory is redundant and is deleted. Colon-scoped sub-namespaces
// (`@namespace("<dir>:...")`), pinned divergences (namespace.pin), and any
// other value stay untouched -- they are load-bearing. Registered with
// memqlmigrate as --rewrite=namespace-default (a pathRewriter: the domain
// comes from the file's directory).
func RewriteRedundantNamespace(domain string, src []byte) ([]byte, error) {
	if domain == "" {
		return src, nil
	}
	re, err := regexp.Compile(`(?m)^@namespace\("` + regexp.QuoteMeta(domain) + `"\)[ \t]*\r?\n`)
	if err != nil {
		return src, nil
	}
	out := re.ReplaceAllString(string(src), "")
	if out == string(src) {
		return src, nil
	}
	out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	return []byte(out), nil
}
