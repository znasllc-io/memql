package memql

import (
	"fmt"
	"regexp"
	"strings"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// useImportRe matches a file-top Form-B `use <path>.{ a, b, c }` import, used
// to discover which concept short-names are in local scope (and the namespace
// hint that disambiguates a trailing-segment collision).
var useImportRe = regexp.MustCompile(`(?m)^\s*use\s+([\w.]+)\.\{([^}]*)\}`)

// declaredNamespaceForDomain returns the namespace a domain's concepts
// actually assemble under: its one-line `namespace.pin` when present (the
// #2614 escape hatch for a DELIBERATE @namespace divergence from the
// directory), otherwise the directory name itself.
//
// This is the plumbing memql#3026 is about. #2976 asked for the ambient check
// to test the DECLARED namespace; #3017 shipped a global-uniqueness test
// instead, on the stated grounds that the pin "needs the tree FS, which the
// loader does not thread this far". It needs no threading: component/memql
// already imports the dsl package (ai_prompts.go, capability_loader.go,
// build_offline_sense.go all do), so the tree is reachable from right here.
//
// dsl.Tree() overlays runtime-mounted product domains (MEMQL_DSL_PATH) on the
// embedded tree, so a bundle's own pin is honoured exactly like a core
// domain's. That matters specifically: the cross-repo ambiguity this rule
// closes arrives from those mounts.
// The read itself delegates to namespacePin, the loader's own pin reader, so
// there is exactly one definition of "what this domain's pin says". Copying
// its four lines here instead would be two sources of truth for one file, and
// the copy is the one that drifts.
func declaredNamespaceForDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return ""
	}
	if pin := namespacePin(memqldsl.Tree(), domain); pin != "" {
		return pin
	}
	return domain
}

// declaredNamespaceForOrigin is declaredNamespaceForDomain for a FILE, and it
// is the form the loader must use, because the two disagree on the nested
// `<domain>/<sub>/*.memql` layout (memql#3026 landing review).
//
// Boot assembles a concept's id from the origin's FIRST path segment --
// unified_loader.go's pass 1 is `dir := firstPathSegment(p)` followed by
// AssembleConceptIdFromDeclInDir(decl, dir, namespacePin(tree, dir)), so
// beta/sub/concepts.memql yields v1:beta:widget (pinned by
// dslimports/lane2_nested_domain_test.go). DomainFromFilePath returns the LAST
// directory segment, "sub", which is not a namespace at all.
//
// Feeding "sub" to the ambient rule refuses every same-domain reference in a
// nested file and demands the same-domain import TestNoSameDomainUse bans --
// the memql#2976 deadlock reinstated one directory deeper. Deriving the
// namespace the way boot derives the id is what keeps the loader and boot
// agreeing, which is the whole point of this rule.
func declaredNamespaceForOrigin(origin string) string {
	return declaredNamespaceForDomain(RootDomainFromFilePath(origin))
}

// idDeclaredNamespaceMatches reports whether a canonical concept id belongs to
// declaredNS -- either as that exact namespace or as a colon EXTENSION of it.
//
// The test is anchored at the id's namespace, not a substring search of the
// whole id (memql#3026 landing review). Namespaces are colon-separated and
// multi-segment ones are live -- dsl/cognition declares
// @namespace("cognition:client:tool") -- so the unanchored
// strings.Contains(id, ":"+declaredNS+":") matched on any INTERIOR segment: a
// bundle domain named "tool" or "client" bound v1:cognition:client:tool:gadget
// ambiently, with no import, on the path that derives row ids. That is exactly
// the cross-namespace bind this rule exists to refuse, and once uniqueness was
// deleted this condition became the only thing refusing it.
//
// The colon extension is admitted deliberately rather than overlooked: it is
// the inverse of assembly. AssembleConceptIdFromDeclInDir accepts an
// @namespace that colon-extends the directory ("colon-extension-wins" in
// concept_id_dir_test.go), so v1:cognition:client:tool:request IS a concept of
// domain cognition and is in its ambient scope.
func idDeclaredNamespaceMatches(id, declaredNS string) bool {
	if id == "" || declaredNS == "" {
		return false
	}
	// Ids are `v<major>:<namespace>:<name>`; drop the version segment so the
	// namespace can be anchored at its own first character.
	rest := id
	if v := strings.IndexByte(rest, ':'); v > 0 {
		rest = rest[v+1:]
	}
	return strings.HasPrefix(rest, declaredNS+":")
}

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
// name is accepted only when the resolved id belongs to this domain's DECLARED
// NAMESPACE -- its namespace.pin when it has one, else the directory. An
// explicit import always wins (the import map is consulted first).
//
// The declared namespace is the rule memql#2976 asked for, and it arrived by
// way of two others that are worth keeping straight, because each was wrong in
// a different direction:
//
//   - the ORIGINAL rule compared the id against the containing DIRECTORY. That
//     refuses any pack whose directory differs from its @namespace --
//     dsl/deployment pins "cluster", so its own concepts assemble as v1:cluster:*
//     and the loader demanded an import for a concept in its own domain, while
//     the import it named was the one TestNoSameDomainUse bans. No spelling
//     worked (#2976).
//   - #3017 replaced it with tree-wide UNIQUENESS, on the argument that boot
//     binds a signature concept the same way. That fixed the remapped pack but
//     WIDENED binding: a concept declared only in a foreign domain bound with
//     no import, on the path that derives row ids. It also left a remapped pack
//     whose name is AMBIGUOUS deadlocked, since uniqueness cannot disambiguate
//     by construction, and let a bundle mounted at MEMQL_DSL_PATH make this
//     tree's reference ambiguous -- an unrelated repository failing this
//     engine's boot (memql#3026).
//
// The declared namespace fixes the remapped pack without widening anything,
// which is why it was the original ask: the hint disambiguates a colliding
// short-name, and #2617's import discipline holds again for foreign concepts.
//
// A name in a foreign namespace, an ambiguous name with no local candidate,
// and a name declared nowhere all still error.
//
// NOTE the domain passed here must be the one the id was ASSEMBLED from. For a
// nested `<domain>/<sub>/*.memql` file that is the first path segment, not the
// last -- the loader therefore calls the InNamespace form below with
// declaredNamespaceForOrigin, and this two-argument wrapper is correct only for
// a flat domain. See declaredNamespaceForOrigin.
func (r *ConceptResolver) ResolveCanonicalIdConceptRefsInDomain(content, domain string) (string, error) {
	return r.ResolveCanonicalIdConceptRefsInNamespace(content, domain, declaredNamespaceForDomain(domain))
}

// ResolveCanonicalIdConceptRefsInNamespace is the namespace-aware form
// (memql#3026). `declaredNS` is the namespace the domain's concepts actually
// assemble under -- its namespace.pin when it has one, else the directory.
// Passing it explicitly is what lets a test build a remapped fixture without
// depending on the real tree; the InDomain wrapper resolves it from the pin.
func (r *ConceptResolver) ResolveCanonicalIdConceptRefsInNamespace(content, domain, declaredNS string) (string, error) {
	imports := importedConceptHints(content)
	if strings.TrimSpace(declaredNS) == "" {
		declaredNS = strings.TrimSpace(domain)
	}

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
		// A comment is PROSE, not code (memql#3026). The scanner tracked
		// string literals but not comments, so a `canonicalId(...)` written in
		// a comment was treated as a real call and the author's comment TEXT
		// was rewritten -- and a comment naming an unknown concept became a
		// hard load error. #3017 papered over this with an authoring warning
		// ("Do NOT write the call form in a comment across a line break")
		// instead of fixing the scanner.
		//
		// BOTH of the lexer's comment forms are skipped, not just `//`
		// (memql#3026 landing review). skipWhitespace in
		// language/parser/lexer.go dispatches to skipLineComment for `//` AND
		// skipBlockComment for `/* */`, so handling only the line form left
		// the identical defect alive in block comments -- including the
		// wrapped-across-a-line-break case the authoring warning was about,
		// which is the one a block comment is most likely to be.
		//
		// Both are copied through verbatim. The inStr check comes first, so a
		// `//` inside a string (a URL in an @description) is not a comment --
		// treating it as one would swallow the rest of the file including real
		// calls.
		if !inStr && strings.HasPrefix(content[i:], "//") {
			end := strings.IndexByte(content[i:], '\n')
			if end < 0 {
				b.WriteString(content[i:])
				i = len(content)
				continue
			}
			b.WriteString(content[i : i+end])
			i += end
			continue
		}
		if !inStr && strings.HasPrefix(content[i:], "/*") {
			// An unterminated block comment runs to end of file, matching
			// skipBlockComment's own behaviour -- the parser reports it, and
			// this scanner must not rewrite the text on the way there.
			end := strings.Index(content[i+2:], "*/")
			if end < 0 {
				b.WriteString(content[i:])
				i = len(content)
				continue
			}
			stop := i + 2 + end + len("*/")
			b.WriteString(content[i:stop])
			i = stop
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
		if !ok && declaredNS != "" {
			// Ambient same-NAMESPACE scope (#2617, corrected by memql#3026).
			//
			// The test is the concept's DECLARED namespace -- the domain's
			// namespace.pin when it has one, else its directory. That is what
			// #2976 asked for, and it is neither of the two rules that came
			// before it:
			//
			//   - the ORIGINAL test compared the id against the containing
			//     DIRECTORY, which is wrong for any pack whose directory
			//     differs from its @namespace. dsl/deployment pins "cluster",
			//     so its concepts assemble as v1:cluster:* while the directory
			//     hint was "deployment" -- the loader demanded an import for a
			//     concept in its own domain, and the import it asked for was
			//     the one TestNoSameDomainUse bans. No spelling worked.
			//
			//   - #3017 replaced it with tree-wide UNIQUENESS, which fixed
			//     that pack but widened binding: a concept declared ONLY in a
			//     foreign domain bound with no import, on the path that
			//     derives row ids. It also left the remapped+AMBIGUOUS case
			//     deadlocked, because uniqueness cannot disambiguate by
			//     construction -- and let a product bundle mounted at
			//     MEMQL_DSL_PATH make this tree's reference ambiguous, so an
			//     unrelated repository could fail this engine's boot.
			//
			// The declared namespace fixes the remapped pack WITHOUT widening
			// cross-domain binding at all, which is why it was the ask. It
			// disambiguates a colliding short-name (the hint filters the
			// candidates), and it refuses a foreign concept whose id does not
			// carry this namespace -- so #2617's import discipline holds again
			// on the id-deriving path.
			if id, aerr := r.resolveBareConceptNameWithNamespace(secondArg, declaredNS); aerr == nil {
				if idDeclaredNamespaceMatches(id, declaredNS) {
					nsHint, ok = declaredNS, true
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
