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

// translateShapeBodyPath rewrites an author-facing shape body path to
// its stored projection path (epic #2292). Since the shape's concept is
// bound by the signature, a payload property is written by BARE name --
// `status` -> the stored `payload.status`. Ambient prefixes are
// preserved:
//
//   - `row.X`        -> `X`             (row intrinsic / metadata)
//   - `row.payload.X`-> `payload.X`     (legacy nested form; authoring
//     it is rejected upstream)
//   - `actor.X`      -> `actor.X`       (auth envelope)
//   - bare `X`       -> `payload.X`     (payload property -- the new
//     default author form)
//
// useConcepts is retained for signature compatibility but unused: the
// legacy `<concept>.X` head form is gone (the concept binds via the
// signature).
func translateShapeBodyPath(path string, useConcepts []string) string {
	_ = useConcepts
	if strings.HasPrefix(path, "row.") {
		return strings.TrimPrefix(path, "row.")
	}
	if strings.HasPrefix(path, "actor.") {
		return path
	}
	if strings.HasPrefix(path, "payload.") {
		// Old explicit form -- rejected at the converter; pass through if
		// it ever reaches here so the stored path stays well-formed.
		return path
	}
	// Bare payload property -> stored payload path.
	return "payload." + path
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
