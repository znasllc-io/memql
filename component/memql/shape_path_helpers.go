package memql

import (
	"strings"
	"unicode"
)

// shape_path_helpers.go carries the two path-rewriting helpers the
// shape converter still calls after #310 deleted shape_parser.go.
// Both helpers are pure-string utilities -- no dependency on the
// retired recursive-descent parser machinery -- which is why they
// survive the wholesale parser sunset.
//
//   - translateShapeBodyPath rewrites concept-bound bare-name path
//     heads (e.g. `space.X` when `space` is a bound concept) to
//     `payload.X`, while leaving row-intrinsic + actor-side paths
//     untouched. Used by the shape converter when emitting the
//     storedPath for each field entry in a ShapeDefinition.
//
//   - pathTerminalKey extracts the trailing dotted segment of a
//     storedPath (`payload.address.city` -> `city`). Used by the
//     converter to derive the template-entry key under which the
//     shape stores each path.
//
//   - isSimpleShapeIdent gates pathTerminalKey: it returns the
//     trailing segment only when it parses as a bare identifier
//     (letters / digits / underscores, starting with a letter or
//     underscore). Paths whose terminal segment isn't a clean
//     identifier (e.g. trailing brackets, dotted-then-numeric
//     subscripts) get a "" terminal so the converter knows to skip
//     the template-entry collapse.

// translateShapeBodyPath rewrites the head of a dotted shape path
// from a bound-concept bare name to `payload`. `row.X` paths drop
// the `row.` prefix; `actor.X` paths pass through; everything else
// is returned unchanged.
func translateShapeBodyPath(path string, useConcepts []string) string {
	if strings.HasPrefix(path, "row.") {
		return strings.TrimPrefix(path, "row.")
	}
	if strings.HasPrefix(path, "actor.") {
		return path
	}
	if idx := strings.Index(path, "."); idx > 0 {
		head := path[:idx]
		for _, c := range useConcepts {
			if head == c {
				return "payload." + path[idx+1:]
			}
		}
	}
	return path
}

// pathTerminalKey returns the terminal identifier segment of a
// dotted path. `payload.displayName` -> `displayName`, `id` -> `id`,
// `payload.address.city` -> `city`. Returns "" when the terminal
// isn't a simple identifier (so the converter knows to skip the
// implicit-key collapse for that path).
func pathTerminalKey(path string) string {
	terminal := path
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		terminal = path[idx+1:]
	}
	if !isSimpleShapeIdent(terminal) {
		return ""
	}
	return terminal
}

// isSimpleShapeIdent reports whether s is a bare identifier: letters
// / digits / underscores, leading character is a letter or
// underscore. Empty string returns false.
func isSimpleShapeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, ch := range s {
		switch {
		case i == 0 && (unicode.IsLetter(ch) || ch == '_'):
			continue
		case i > 0 && (unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_'):
			continue
		default:
			return false
		}
	}
	return true
}
