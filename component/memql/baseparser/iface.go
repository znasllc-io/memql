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
// Returns nil when every top-level `@name` is in the allow-list.
// Returns an error citing the offending name + the full allow-list
// on the first mismatch.
func ValidateConstructAnnotations(source, kindLabel string, allowed map[string]bool) error {
	bodyStart := findConstructBodyOpen(source, kindLabel)
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
