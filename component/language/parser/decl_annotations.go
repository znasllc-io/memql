package parser

import (
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// validateDeclAnnotations rejects unknown leading annotations on the
// declarative construct kinds that previously tolerated them silently --
// shape / spec / trait / builtin / prompt / policy (HOLE 1 of memql#2395,
// surfaced by the #2383 negative-syntax suite). tool / provider / seed
// already enforce their sets inline in their decl parsers, and the four
// function kinds (query / mutation / logic / automation) are enforced by
// the load-time validator in component/memql.
//
// The allow-list is the canonical annotations.ByReceiver registry (#991's
// single physical source of truth; a leaf package, so this import adds no
// cycle). Traits validate against the "Spec" receiver set -- parseSpecDecl
// serves both kinds and they accept the same lifecycle/description set.
func (p *Parser) validateDeclAnnotations(receiver, kindWord, name string, attrs []*Attribute) error {
	allowed := annotations.ByReceiver[receiver]
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	for _, attr := range attrs {
		if attr == nil || set[attr.Name] {
			continue
		}
		sorted := append([]string(nil), allowed...)
		sort.Strings(sorted)
		return newParseErrorf(&p.current,
			"%s %q: unknown annotation @%s%s -- supported: @%s",
			kindWord, name, attr.Name,
			didYouMean(attr.Name, sorted),
			strings.Join(sorted, ", @"))
	}
	return nil
}
