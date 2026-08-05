package memql

import (
	"fmt"
	"io"
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
func declaredNamespaceForDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return ""
	}
	f, err := memqldsl.Tree().Open(domain + "/namespace.pin")
	if err != nil {
		return domain
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return domain
	}
	if pin := strings.TrimSpace(string(b)); pin != "" {
		return pin
	}
	return domain
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
		// A `//` comment (and its `///` doc form) is PROSE, not code
		// (memql#3026). The scanner tracked string literals but not comments,
		// so a `canonicalId(...)` written in a comment was treated as a real
		// call and the author's comment TEXT was rewritten -- and a comment
		// naming an unknown concept became a hard load error. #3017 papered
		// over this with an authoring warning ("Do NOT write the call form in
		// a comment across a line break") instead of fixing the scanner.
		//
		// Copied through verbatim to end-of-line. The inStr check comes first,
		// so a `//` inside a string (a URL in an @description) is not a
		// comment -- treating it as one would swallow the rest of the file
		// including real calls.
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
				if strings.Contains(id, ":"+declaredNS+":") {
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
