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
			b.WriteString(content[i:openParen+1])
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
		if !ok {
			return "", fmt.Errorf("canonicalId: concept %q is not imported -- add a file-top `use <ns>.concepts.{ %s }` import (or use the canonical-id string form)", secondArg, secondArg)
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
