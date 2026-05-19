package memql

import (
	"fmt"
	"regexp"
	"strings"
)

// legacyConceptPathRefRE matches a `<bareName>.` token preceded by a
// word boundary. The bareName is one or more letters / digits /
// underscores. The trailing `.` is required so we only match field
// accesses (`agent.X`), not bare references (`@useConcept(agent)`).
//
// The pattern is rebuilt per call inside detectLegacyConceptPathRefs
// because the bareName is parameterised; this prepared pattern is
// only used by the comment-stripping helper.
var multiLineCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

// detectLegacyConceptPathRefs returns a slice of line-anchored
// snippets where `<bareName>.X` appears as a field-access token in
// the source, EXCLUDING:
//
//   - the `@useConcept(<bareName>)` annotation line itself (and
//     other `@use*(<bareName>)` annotation lines) -- there the bare
//     name is the argument, not a field access
//   - lines that start with `//` (line comments)
//   - block comments `/* ... */`
//   - text inside double-quoted string literals
//
// When this returns a non-empty slice, the file was authored using
// the legacy filter form (`agent.X==args.X`) instead of the
// canonical form (`payload.X==args.X`). The loader treats this as
// a load-time error so the rule is enforced beyond the conformance
// test (which only runs at `go test` time).
//
// The detection is deliberately conservative: it skips the
// annotation line and comments but does NOT try to be context-aware
// about whether the match sits inside a `filter` clause vs an
// `insert` block vs a procedural-form `shape(...)`. The migration
// applied the same rule everywhere, so the rejection is global too.
func detectLegacyConceptPathRefs(source, bareName string) []string {
	// Strip block comments first; line-comment stripping happens
	// per-line below.
	stripped := multiLineCommentRE.ReplaceAllString(source, "")
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(bareName) + `\.`)

	var hits []string
	for lineNo, raw := range strings.Split(stripped, "\n") {
		line := raw
		// Strip the trailing `//` comment (best-effort; don't try
		// to handle `//` inside string literals).
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		// Skip @useConcept / @useShape / @useTrait / @useSpec /
		// @useMutation / @useQuery / @useAutomation / @useBuiltin /
		// @useLogic / @useTool / @usePrompt / @useProvider lines:
		// the bare name there is an argument, not a field access.
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "@use") {
			continue
		}
		// Mask string literals so `"agent.X"` doesn't count.
		masked := maskStringLiterals(line)
		if pattern.MatchString(masked) {
			hits = append(hits, fmt.Sprintf("line %d: %s", lineNo+1, strings.TrimSpace(raw)))
		}
	}
	return hits
}

// maskStringLiterals replaces every double-quoted span with a run
// of spaces of the same length. Single-quoted spans aren't used in
// MemQL string literals so this only handles `"..."`.
func maskStringLiterals(line string) string {
	out := []byte(line)
	inStr := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if c == '\\' && inStr && i+1 < len(out) {
			out[i] = ' '
			out[i+1] = ' '
			i++
			continue
		}
		if c == '"' {
			inStr = !inStr
			out[i] = ' '
			continue
		}
		if inStr {
			out[i] = ' '
		}
	}
	return string(out)
}

// validateNoLegacyConceptPathRefs runs detectLegacyConceptPathRefs
// for every declared @useConcept / @useShape bareName in the source
// and aggregates the hits into a single error if any match. Called
// from the function loader BEFORE translateConceptPathsToPayload so
// authors see a clear "use payload.X" message instead of silent
// translation.
func validateNoLegacyConceptPathRefs(source, origin string) error {
	names := append([]string{}, extractAllUseConceptNames(source)...)
	names = append(names, extractAllUseShapeNames(source)...)
	if len(names) == 0 {
		return nil
	}
	var allHits []string
	for _, name := range names {
		for _, hit := range detectLegacyConceptPathRefs(source, name) {
			allHits = append(allHits, fmt.Sprintf("  %s.X reference at %s", name, hit))
		}
	}
	if len(allHits) == 0 {
		return nil
	}
	return fmt.Errorf("legacy filter syntax in %s: the file binds @useConcept / @useShape but its body still references payload fields via `<conceptName>.X`. Use `payload.X` instead.\n%s",
		originLabel(origin), strings.Join(allHits, "\n"))
}

func originLabel(origin string) string {
	if origin == "" {
		return "<unknown>"
	}
	return origin
}
