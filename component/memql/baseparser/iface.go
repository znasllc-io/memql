package baseparser

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateConstructAnnotations hard-rejects unknown annotations on a
// construct source. kindLabel is the construct keyword used in the
// error message; allowed is the construct's allow-list.
//
// Also hard-rejects the legacy @use* family of annotations
// (@useConcept / @useShape / @useQuery / @useMutation / @useLogic /
// @useBuiltin / @useTrait / @useSpec / @useTool / @usePrompt /
// @useProvider / @useAutomation) with a migration-pointing error
// message. These were retired in the import-model pivot in favour of
// file-top `use <module>.{ names }` imports plus signature-bound
// concept binding for seeds / queries / mutations / shapes.
//
// Returns nil when every top-level `@name` is in the allow-list and
// no @use* annotation is present.

// retiredConstructAnnotations maps construct-level annotations hard-retired
// under the 2026.08 epoch to their migration hints, checked BEFORE the
// allow-list (the @use* precedent) so the author gets the pointed retirement
// message rather than a generic unknown-annotation error. Field-level
// annotations (the concept property fold: @secret / @pii / @internal on
// FIELDS) are a separate surface and are not consulted here.
var retiredConstructAnnotations = map[string]string{
	"internal": "retired under the 2026.08 epoch (#2620 ruling / #2708); it only hid the construct from external discovery surfaces (tool listing, MCP promotion, the help()/listFunctions internal flag) while leaving it callable -- delete the annotation",
	"role":     "buried (#2631 ruling / #2709); it was documented but never enforced (the load gate always rejected it, and nothing checked the value at runtime) -- access control lives at the actor layer (RBAC + the @public per-row-authz classification)",
}

// RetiredConstructAnnotation reports whether a construct-level annotation
// name is hard-retired, returning its migration hint. Exported so every
// annotation gate (this validator, the parser's declarative-kind validator,
// the sense editor diagnostics) emits the same pointed message instead of a
// generic unknown-annotation error.
func RetiredConstructAnnotation(name string) (string, bool) {
	hint, ok := retiredConstructAnnotations[name]
	return hint, ok
}

func ValidateConstructAnnotations(source, kindLabel string, allowed map[string]bool) error {
	keyword := kindLabel
	if kindLabel == "mutation" {
		// Mutation slices spell the header `mutate NAME {`; without the
		// keyword mapping the header scan would run past the body open
		// and inspect body lines too.
		keyword = "mutate"
	}
	bodyStart := findConstructBodyOpen(source, keyword)
	if bodyStart < 0 {
		bodyStart = len(source)
	}
	header := source[:bodyStart]

	for _, raw := range strings.Split(header, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "@") {
			continue
		}
		name := extractAnnotationIdent(line)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "use") && len(name) > 3 && name[3] >= 'A' && name[3] <= 'Z' {
			return fmt.Errorf("`@%s(...)` is retired -- declare the dependency via a file-top `use <module>.{ ... }` import instead, and (for seeds/queries/mutations/shapes) put the bound concept in the signature (`%s <Concept> <name> { ... }`)", name, kindLabel)
		}
		if hint, retired := retiredConstructAnnotations[name]; retired {
			return fmt.Errorf("@%s on a %s is retired -- %s", name, kindLabel, hint)
		}
		if !allowed[name] {
			return fmt.Errorf("unknown %s annotation @%s -- supported: %s", kindLabel, name, FormatAnnotationAllowList(allowed))
		}
	}
	return nil
}

// FormatAnnotationAllowList renders the allow-list as a sorted
// comma-separated string for use in error messages.
func FormatAnnotationAllowList(allowed map[string]bool) string {
	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, "@"+n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// findConstructBodyOpen returns the byte index of the `{` that opens
// the construct's body. Looks for the keyword line and returns the
// position of its `{`. Returns -1 when no body-open is found.
func findConstructBodyOpen(source, keyword string) int {
	for offset := 0; offset < len(source); {
		nl := strings.IndexByte(source[offset:], '\n')
		var line string
		var lineStart int
		if nl < 0 {
			line = source[offset:]
			lineStart = offset
			offset = len(source)
		} else {
			line = source[offset : offset+nl]
			lineStart = offset
			offset += nl + 1
		}
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, keyword+" ") || strings.HasPrefix(trimmed, keyword+"\t") {
			braceIdx := strings.IndexByte(line, '{')
			if braceIdx < 0 {
				return -1
			}
			return lineStart + braceIdx
		}
	}
	return -1
}

// extractAnnotationIdent extracts the `name` from `@name(...)` or
// `@name`. Stops at the first non-identifier character.
func extractAnnotationIdent(line string) string {
	if !strings.HasPrefix(line, "@") {
		return ""
	}
	rest := line[1:]
	for i, r := range rest {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' {
			continue
		}
		return rest[:i]
	}
	return rest
}
