package id

import "strings"

// ValidateShortId enforces the two legitimate shapes for a node's
// shortId at mint/insert time. It is the single source of truth for
// the rule that the storage gate (memory-nodes Concept.validateShortId)
// and every system-automation id generator must agree on.
//
// A colon-bearing compound string used as a "shortId" is the bug this
// guards against: the storage layer's HasPartition() check would
// misclassify the first colon-segment as a partition name and persist a
// malformed id. See docs/public/concepts/identifiers.md ("Anti-patterns").
//
// Two legitimate shapes (conceptName may be "" to validate a bare id
// with no concept context):
//
//  1. Bare shortId with no colons -- the engine prepends {concept}:.
//  2. Concept-qualified: starts with conceptName+":".
//
// Anything else is a caller bug. An empty (or whitespace-only) shortId
// is treated as valid here -- it is handled downstream.
func ValidateShortId(conceptName, shortId string) error {
	trimmed := strings.TrimSpace(shortId)
	if trimmed == "" {
		return nil // empty handled downstream
	}
	// Shape (1): bare, no colons -> ok.
	if !strings.ContainsRune(trimmed, ':') {
		return nil
	}
	// Shape (2): concept-qualified.
	if conceptName != "" && strings.HasPrefix(trimmed, conceptName+":") {
		return nil
	}
	return &InvalidShortIdError{ConceptName: conceptName, ShortId: trimmed}
}

// InvalidShortIdError is returned by ValidateShortId for a shortId that
// is neither a bare slug/UUID nor the concept-prefixed form.
type InvalidShortIdError struct {
	ConceptName string
	ShortId     string
}

func (e *InvalidShortIdError) Error() string {
	qualified := "<concept>:<short>"
	if e.ConceptName != "" {
		qualified = e.ConceptName + ":<short>"
	}
	return "shortId " + quote(e.ShortId) +
		" must be a bare slug/UUID (no colons) or the concept-prefixed form (" +
		quote(qualified) + "); got something else (see docs/public/concepts/identifiers.md)"
}

func quote(s string) string { return "\"" + s + "\"" }

// IsValidShortId reports whether shortId satisfies ValidateShortId for
// the given concept (conceptName may be "" for a bare-id check).
func IsValidShortId(conceptName, shortId string) bool {
	return ValidateShortId(conceptName, shortId) == nil
}

// NewSystemNodeShortId builds a collision-free, validator-safe BARE
// shortId for a row minted by a system automation (the reactive loop,
// the refresh cron, etc.). It is the single helper such call sites must
// use instead of hand-rolling `fmt.Sprintf("prefix:%s:%d", ...)`, which
// produces a colon-bearing string the engine rejects (issue #1712).
//
// label is a short provenance tag ("proactive", "refresh"); each
// context segment adds human-readable lineage (a related id, a name).
// Every segment is reduced to its last colon-segment and slugified
// (colons stripped), then dash-joined, with a fresh opaque token
// appended so concurrent mints never collide. The result NEVER contains
// a colon, so it always satisfies ValidateShortId for any concept.
//
//	NewSystemNodeShortId("proactive", "v1:planner:responsibility:d6ac657f")
//	  -> "proactive-d6ac657f-<uuid>"
func NewSystemNodeShortId(label string, context ...string) string {
	parts := slugSegments(label, context)
	parts = append(parts, NewShortId())
	return strings.Join(parts, "-")
}

// SystemNodeShortId builds a DETERMINISTIC, validator-safe BARE shortId
// from label + context with NO uniqueness token -- use it when the id
// must be idempotent across re-invocations (exactly one row per logical
// key, e.g. one training run per approved plan). Same input -> same id.
//
//	SystemNodeShortId("train", "v1:planner:plan:abc-123")
//	  -> "train-abc-123"
func SystemNodeShortId(label string, context ...string) string {
	parts := slugSegments(label, context)
	return strings.Join(parts, "-")
}

// slugSegments reduces label + context into the non-empty, colon-free
// slug segments shared by both system-id generators.
func slugSegments(label string, context []string) []string {
	parts := make([]string, 0, len(context)+2)
	if s := Slugify(lastSegment(label)); s != "" {
		parts = append(parts, s)
	}
	for _, c := range context {
		if s := Slugify(lastSegment(c)); s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}

// lastSegment returns the substring after the final colon, so a full
// canonical id ("v1:planner:responsibility:abc") collapses to its
// readable tail ("abc") before slugification. No colon -> returned as-is.
func lastSegment(s string) string {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}
