package id

import (
	"strings"
	"unicode"
)

// Slugify normalizes a name into the canonical kebab-case shortId
// form used for memQL catalog rows (concepts whose identity is a
// stable human-chosen name -- v1:agents:agentRole,
// v1:common:knowledgeDomain, v1:cluster:nodeType, etc.).
//
// Rules:
//   - Insert a dash on each lowercase->uppercase boundary so
//     camelCase identifiers split into words ("plannerAgent" ->
//     "planner-agent").
//   - Lowercase every letter.
//   - Replace runs of whitespace, underscore, dot, or slash with a
//     single dash.
//   - Drop any character that isn't a lowercase ASCII letter, digit,
//     or dash.
//   - Collapse consecutive dashes into one.
//   - Trim leading + trailing dashes.
//
// Empty input + input that contains nothing slug-worthy both return
// "". Callers that need a guaranteed-non-empty id should fall back
// to NewShortId() in that case.
//
// Examples:
//
//	Slugify("Row Crop Farmer")   -> "row-crop-farmer"
//	Slugify("system_planner")    -> "system-planner"
//	Slugify("plannerAgent")      -> "planner-agent"
//	Slugify("  hello  world  ")  -> "hello-world"
//	Slugify("foo___bar")         -> "foo-bar"
//	Slugify("--leading-trail--") -> "leading-trail"
//	Slugify("ümlaut")            -> "mlaut"  (non-ASCII dropped; pick
//	                                          an ASCII name)
//
// Slugify is intentionally one-way -- the inverse mapping does not
// exist (multiple inputs slugify to the same output). Catalog rows
// store the original human-readable name in a payload field;
// `id` is the slug.
func Slugify(s string) string {
	if s == "" {
		return ""
	}

	out := make([]byte, 0, len(s))
	prevDash := true // suppress leading dash
	var prevLower bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			// Insert a word-boundary dash on lowercase->uppercase
			// transitions so camelCase splits cleanly.
			if prevLower && !prevDash {
				out = append(out, '-')
			}
			out = append(out, byte(r-'A'+'a'))
			prevDash = false
			prevLower = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, byte(r))
			prevDash = false
			prevLower = r >= 'a' && r <= 'z'
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ' ' || unicode.IsSpace(r):
			if !prevDash {
				out = append(out, '-')
				prevDash = true
			}
			prevLower = false
		default:
			// Drop everything else (non-ASCII letters, punctuation).
			prevLower = false
		}
	}

	// Trim a trailing dash (the loop only suppresses leading dashes).
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}

	return strings.TrimSpace(string(out))
}
