package memql

import (
	"fmt"
	"regexp"
	"strings"
)

// useImportRe matches a file-top Form-B `use <path>.{ a, b, c }` import, used
// to discover which concept short-names are in local scope (and the namespace
// hint that disambiguates a trailing-segment collision).
var useImportRe = regexp.MustCompile(`(?m)^\s*use\s+([\w.]+)\.\{([^}]*)\}`)

// importedConceptHints scans a source file's `use` declarations and returns a
// map of imported short-name -> namespace hint (the leading segment of the
// import path, e.g. "cognition" from `use cognition.concepts.{ space }`).
func importedConceptHints(content string) map[string]string {
	out := map[string]string{}
	for _, m := range useImportRe.FindAllStringSubmatch(content, -1) {
		path := m[1]
		ns := path
		if i := strings.IndexByte(path, '.'); i >= 0 {
			ns = path[:i]
		}
		for _, name := range strings.Split(m[2], ",") {
			name = strings.TrimSpace(name)
			// Form-A-style `x as y` aliasing is not used for concept imports,
			// but tolerate it by taking the local binding (after `as`).
			if i := strings.Index(name, " as "); i >= 0 {
				name = strings.TrimSpace(name[i+4:])
			}
			if name != "" {
				out[name] = ns
			}
		}
	}
	return out
}

// ResolveCanonicalIdConceptRefs rewrites the typed foreign-concept form
// `canonicalId(<value>, <importedConceptName>)` to the canonical-id string form
// `canonicalId(<value>, "v1:ns:name")` (#987), resolving the short-name against
// the file's `use ...concepts.{ ... }` imports + the concept registry. The
// quoted string form is left untouched (additive), and `canonicalId(` text
// inside string literals (e.g. an `@description`) is skipped. An unimported or
// unknown concept name is a hard error so the typo surfaces at load.
func (r *ConceptResolver) ResolveCanonicalIdConceptRefs(content string) (string, error) {
	return r.ResolveCanonicalIdConceptRefsInDomain(content, "")
}

// ResolveCanonicalIdConceptRefsInDomain is ResolveCanonicalIdConceptRefs with
// the #2617 ambient rule: when no file-top import names the concept, the bare
// name is resolved against the registry and accepted when it is UNAMBIGUOUS.
// An explicit import always wins (the import map is consulted first).
//
// Read that carefully, because it is wider than the rule this comment used to
// describe (memql#2976). It said resolution was "accepted only when the
// resolved id actually lives in that domain -- cross-domain concepts still
// require the explicit import". That is no longer true: a concept declared
// ONLY in a foreign domain now binds ambiently, with no import, provided its
// trailing segment is unique tree-wide. `canonicalId(x, user)` in an unrelated
// pack rewrites to v1:identity:user rather than failing.
//
// That matches how boot binds a signature concept -- resolveBareConceptName-
// WithNamespace returns a unique trailing-segment match before it consults the
// hint at all -- so the two agree, which is what #2976 was about. But it is
// NOT what #2976 asked for. It asked for the check to test the concept's
// DECLARED NAMESPACE rather than its containing directory, which would have
// fixed the remapped pack without widening cross-domain binding at all. The
// declared namespace is not reachable here (it needs the tree FS for
// namespace.pin, which the loader does not thread this far), so uniqueness
// shipped instead. Consequences, tracked in memql#3026:
//
//   - a namespace-remapped pack whose concept name is AMBIGUOUS is still in
//     the original deadlock -- ambient refuses, and the import the error asks
//     for is the one TestNoSameDomainUse bans;
//   - a product bundle mounted at MEMQL_DSL_PATH that declares a name this
//     tree also declares can retroactively make an ambient reference
//     ambiguous, so the failure arrives from an unrelated repository.
//
// An ambiguous name still errors, and a name declared nowhere still errors.
func (r *ConceptResolver) ResolveCanonicalIdConceptRefsInDomain(content, domain string) (string, error) {
	imports := importedConceptHints(content)

	var b strings.Builder
	i := 0
	inStr := false
	for i < len(content) {
		c := content[i]
		if c == '"' {
			inStr = !inStr
			b.WriteByte(c)
			i++
			continue
		}
		if inStr || !strings.HasPrefix(content[i:], "canonicalId") {
			b.WriteByte(c)
			i++
			continue
		}
		// Match `canonicalId` followed (across optional whitespace) by `(`.
		j := i + len("canonicalId")
		k := j
		for k < len(content) && (content[k] == ' ' || content[k] == '\t') {
			k++
		}
		if k >= len(content) || content[k] != '(' {
			b.WriteString(content[i:j])
			i = j
			continue
		}
		openParen := k
		// Scan the arg list, tracking paren depth + strings.
		depth := 1
		commaIdx := -1
		argInStr := false
		p := openParen + 1
		for p < len(content) && depth > 0 {
			ch := content[p]
			switch {
			case ch == '"':
				argInStr = !argInStr
			case argInStr:
			case ch == '(':
				depth++
			case ch == ')':
				depth--
			case ch == ',' && depth == 1 && commaIdx == -1:
				commaIdx = p
			}
			p++
		}
		if depth != 0 || commaIdx == -1 {
			// Malformed / single-arg -- leave it for the parser to flag.
			b.WriteString(content[i : openParen+1])
			i = openParen + 1
			continue
		}
		closeParen := p - 1
		value := content[openParen+1 : commaIdx]
		secondArg := strings.TrimSpace(content[commaIdx+1 : closeParen])

		if strings.HasPrefix(secondArg, "\"") {
			// String form -- additive, leave unchanged.
			b.WriteString(content[i : closeParen+1])
			i = closeParen + 1
			continue
		}

		nsHint, ok := imports[secondArg]
		if !ok && domain != "" {
			// Ambient same-domain scope (#2617): accept the bare name when
			// boot would bind it without an import.
			//
			// The test used to be `strings.Contains(id, ":"+domain+":")` --
			// the id against the containing DIRECTORY. That is wrong for any
			// pack whose directory differs from its @namespace, which is a
			// supported shape (@namespace exists precisely so a directory need
			// not dictate the canonical id). dsl/deployment declares
			// @namespace("cluster"), so its concepts assemble to
			// v1:cluster:deployment while the ambient hint is "deployment" --
			// ":deployment:" is not in that id, and the loader told the author
			// to import a concept declared in the very same domain. Worse,
			// there was no spelling that worked: the same-domain import
			// TestNoSameDomainUse forbids was the one the error asked for
			// (memql#2976).
			//
			// Uniqueness is the honest condition, and it is what boot already
			// uses. resolveBareConceptNameWithNamespace returns a unique
			// trailing-segment match BEFORE it ever consults the hint, so
			// signature-concept binding has always accepted these names
			// ambiently -- only canonicalId carried the extra directory test.
			// Removing that asymmetry is the fix.
			//
			// An AMBIGUOUS name still requires an import unless the directory
			// really does name the namespace: two concepts sharing a trailing
			// segment is exactly the case #2617's rule protects, and dropping
			// the guard entirely would let a cross-domain collision bind
			// silently to whichever the hint happened to favour.
			if id, aerr := r.resolveBareConceptNameWithNamespace(secondArg, domain); aerr == nil {
				if _, uerr := r.resolveBareConceptNameWithNamespace(secondArg, ""); uerr == nil {
					nsHint, ok = domain, true // unique in the tree
				} else if strings.Contains(id, ":"+domain+":") {
					nsHint, ok = domain, true // ambiguous, but same-domain by directory
				}
			}
		}
		if !ok {
			return "", fmt.Errorf("canonicalId: concept %q is neither imported nor a same-domain concept -- add a file-top `use <ns>.concepts.{ %s }` import (or use the canonical-id string form)", secondArg, secondArg)
		}
		canonicalID, err := r.resolveBareConceptNameWithNamespace(secondArg, nsHint)
		if err != nil {
			return "", fmt.Errorf("canonicalId concept %q: %w", secondArg, err)
		}

		b.WriteString("canonicalId(")
		b.WriteString(value)
		b.WriteString(", \"")
		b.WriteString(canonicalID)
		b.WriteString("\")")
		i = closeParen + 1
	}
	return b.String(), nil
}
