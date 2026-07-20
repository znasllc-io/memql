package parser

import (
	"regexp"
	"strings"
)

// RewriteSameDomainUse deletes file-top `use` declarations that import
// from the file's OWN domain (#2617): same-domain constructs are
// ambient -- in scope with no import -- so the line is pure ceremony.
// Cross-domain imports are untouched, and the explicit same-domain
// form keeps parsing (the rule is additive; this is the codemod, not
// a rejection). The caller derives `domain` from the file's containing
// directory. Multi-line brace lists are handled; a deleted line takes
// its trailing newline with it.
func RewriteSameDomainUse(domain string, src []byte) ([]byte, error) {
	if domain == "" {
		return src, nil
	}
	re, err := regexp.Compile(`(?m)^[ \t]*use[ \t]+` + regexp.QuoteMeta(domain) + `\.[\w.]*[ \t]*\{[^}]*\}[ \t]*\r?\n?`)
	if err != nil {
		return src, nil
	}
	out := re.ReplaceAllString(string(src), "")
	if out == string(src) {
		return src, nil
	}
	// Collapse a doubled blank gap the deletion may leave behind.
	out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	return []byte(out), nil
}
