package parser

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The data-origins declaration surface (epic memql#4378): what MemQL's
// relationship is to the data a concept holds. Two annotations produce
// three states, and the three states are the whole vocabulary --
// there is deliberately no fourth.
//
//	@origin("shopify")            concept product { ... }            // mirror
//	@origin("memql")
//	@mirroredTo("shopify")        concept creditLimit { ... }        // origin
//	                              concept plan { ... }               // native
//
// The sentence the docs carry: MemQL is the origin of what it owns, a
// faithful mirror of what it does not, and every concept says which.
//
// THIS FILE IS THE DECLARATION SURFACE, AND WHAT IT PRODUCES IS
// ENFORCED. component/memql/mirror_write_guard.go refuses every write to
// a mirror concept that does not come from the connector its origin
// names -- users, agents, tool handlers, raw inserts and staged writes
// alike -- and component/memql/rowauthz_connector.go admits a connector
// actor to exactly the concepts that name it. So a mis-parse here is an
// authorization outcome, not a lost annotation, which is the same
// property rowauthz_binding.go carries and the reason both live beside
// the parser rather than inside a consumer.
//
// The detector and the renderer share this file for the #2621 reason
// the row-authz binding states: the loader-side validator
// (component/database/memory-nodes) and anything that WRITES a
// declaration must share one definition of what a declaration means, or
// they drift into disagreeing about the same annotation.

// OriginAnnotation and MirroredToAnnotation are the annotation names,
// spelled once.
const (
	OriginAnnotation     = "origin"
	MirroredToAnnotation = "mirroredTo"
)

// OriginMemQL is the reserved origin name meaning "MemQL originates
// this concept". It is the DEFAULT -- a concept with no @origin is
// MemQL-origin -- and writing it explicitly is what an author does when
// they also declare @mirroredTo and want the pair to read as one
// statement.
//
// It is reserved: no connector may be named "memql", because a
// connector by that name would make `@origin("memql")` ambiguous
// between "MemQL owns this" and "the connector called memql fills it",
// and an ambiguous origin is the one thing this vocabulary exists to
// prevent.
const OriginMemQL = "memql"

// DataState is the DERIVED relationship between MemQL and a concept's
// data. It is never authored: an author writes @origin and @mirroredTo,
// and the state falls out of the pair (D2). Three values, no fourth --
// "shared", two systems both authoring one domain with conflict rules,
// is the option the model rejects and the vocabulary cannot say.
type DataState string

const (
	// DataStateMirror -- the origin is external. Changes are made
	// there; MemQL holds a faithful copy and is READ-ONLY BY
	// CONSTRUCTION (D3).
	DataStateMirror DataState = "mirror"
	// DataStateOrigin -- MemQL originates the data and at least one
	// external system holds a mirror, kept in sync outbound through the
	// durable outbox (D5).
	DataStateOrigin DataState = "origin"
	// DataStateNative -- MemQL originates the data and nobody else
	// holds a copy. The default, and by far the common case: plans,
	// agents, audit, memories, constructs.
	DataStateNative DataState = "native"
)

// OriginDecl is the parsed meaning of a concept's @origin +
// @mirroredTo pair.
//
// The zero value is a valid declaration: it means NATIVE. That is
// deliberate -- an undeclared concept is native, and a nil-vs-zero
// distinction here would invent a fourth state for "did not say".
type OriginDecl struct {
	// Origin is the connector name that owns the data, or OriginMemQL.
	// Empty means the same as OriginMemQL: absent @origin defaults to
	// MemQL (D2).
	Origin string `json:"origin,omitempty"`
	// MirroredTo names the connectors this concept's changes propagate
	// OUT to. Only meaningful when the origin is MemQL; a mirror may
	// not be mirrored onward from here (re-mirroring is the origin's
	// job), which ValidateOriginDecl refuses.
	MirroredTo []string `json:"mirroredTo,omitempty"`
}

// IsMemQLOrigin reports whether MemQL originates this concept. Absent
// and explicit "memql" are the same answer, which is what lets an
// author write the annotation for readability without changing meaning.
func (d OriginDecl) IsMemQLOrigin() bool {
	o := strings.TrimSpace(d.Origin)
	return o == "" || o == OriginMemQL
}

// State derives the concept's DataState from the declaration. This is
// THE derivation -- the registry, the SDKs, the write guard and the
// portal badge all read the state through this one function, so there
// is no second place for the three states to be computed differently.
func (d OriginDecl) State() DataState {
	if !d.IsMemQLOrigin() {
		return DataStateMirror
	}
	if len(d.MirroredTo) > 0 {
		return DataStateOrigin
	}
	return DataStateNative
}

// Connectors returns every connector name the declaration NAMES, in a
// stable order -- the origin when it is external, plus every mirror
// target. This is the set the boot check resolves against the connector
// registry: a name nobody serves is a mirror with nobody to fill it or
// a mirror target nobody drains, and both are silent data loss (D2).
func (d OriginDecl) Connectors() []string {
	var out []string
	if !d.IsMemQLOrigin() {
		out = append(out, strings.TrimSpace(d.Origin))
	}
	out = append(out, d.MirroredTo...)
	sort.Strings(out)
	return out
}

// connectorNamePattern is the FORM a connector name must take: a
// lowerCamelCase identifier, the same shape the @relationship `as`
// label takes (memql#3652).
//
// Validated for form only. Membership is a separate question, asked at
// boot against the connector registry rather than here, because this
// package is a leaf the registry cannot reach -- and because a name's
// SHAPE is a language fact while its EXISTENCE is a deployment fact.
// Conflating them would put the set of shippable connectors inside the
// parser, which is exactly the treadmill the relationship-type split
// exists to avoid.
var connectorNamePattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

// validConnectorName reports whether s is a well-formed connector name.
func validConnectorName(s string) bool {
	return connectorNamePattern.MatchString(s)
}

// ParseOrigin turns an `@origin("...")` attribute into the origin name
// it declares, or into the diagnostic explaining why it declares none.
//
// The returned error never names the concept -- the caller holds that
// and wraps, the way applyConceptAttribute's siblings do.
func ParseOrigin(attr *Attribute) (string, error) {
	if attr == nil {
		return "", fmt.Errorf("@%s: nil attribute", OriginAnnotation)
	}
	if attr.Name != OriginAnnotation {
		return "", fmt.Errorf("@%s: called on @%s", OriginAnnotation, attr.Name)
	}
	if len(attr.Args) > 0 {
		return "", fmt.Errorf(
			"@%s takes a single quoted name, not keyword arguments -- write @%s(%q) or @%s(%q)",
			OriginAnnotation, OriginAnnotation, OriginMemQL, OriginAnnotation, "shopify")
	}
	name, ok := attr.Value.(string)
	if !ok {
		// A multi-value list lands here as []string. A concept has
		// exactly ONE origin: two origins is not a wider statement, it
		// is an unanswerable question about where a change is made.
		if _, isList := attr.Value.([]string); isList {
			return "", fmt.Errorf(
				"@%s takes exactly one name -- a concept has one origin. For MemQL-origin data mirrored outward, write @%s(%q) with @%s(...)",
				OriginAnnotation, OriginAnnotation, OriginMemQL, MirroredToAnnotation)
		}
		return "", fmt.Errorf("@%s requires a quoted connector name -- write @%s(%q)",
			OriginAnnotation, OriginAnnotation, "shopify")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("@%s(\"\") is empty -- name the system where changes to this concept are made, or %q for MemQL itself",
			OriginAnnotation, OriginMemQL)
	}
	if name == OriginMemQL {
		return OriginMemQL, nil
	}
	if !validConnectorName(name) {
		return "", fmt.Errorf("@%s(%q): %s", OriginAnnotation, name, connectorNameFormHint(name))
	}
	return name, nil
}

// ParseMirroredTo turns a `@mirroredTo("a", "b")` attribute into the
// connector names it declares. Order is preserved as authored, so the
// outbox appends entries in the order the author listed the targets.
func ParseMirroredTo(attr *Attribute) ([]string, error) {
	if attr == nil {
		return nil, fmt.Errorf("@%s: nil attribute", MirroredToAnnotation)
	}
	if attr.Name != MirroredToAnnotation {
		return nil, fmt.Errorf("@%s: called on @%s", MirroredToAnnotation, attr.Name)
	}
	if len(attr.Args) > 0 {
		return nil, fmt.Errorf(
			"@%s takes quoted connector names, not keyword arguments -- write @%s(%q)",
			MirroredToAnnotation, MirroredToAnnotation, "shopify")
	}

	var names []string
	switch v := attr.Value.(type) {
	case string:
		names = []string{v}
	case []string:
		names = append(names, v...)
	default:
		return nil, fmt.Errorf("@%s requires at least one quoted connector name -- write @%s(%q)",
			MirroredToAnnotation, MirroredToAnnotation, "shopify")
	}

	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("@%s(\"\") is empty -- name the system that holds the mirror",
				MirroredToAnnotation)
		}
		if name == OriginMemQL {
			// MemQL mirroring to itself is not a statement about
			// anything. Refused rather than dropped, because dropping it
			// would leave a concept reading as an origin in source and
			// deriving as native in the registry.
			return nil, fmt.Errorf(
				"@%s(%q): %q is MemQL itself, not a mirror target -- a concept cannot be mirrored to its own origin. Name an external connector, or drop the annotation to leave the concept native",
				MirroredToAnnotation, OriginMemQL, OriginMemQL)
		}
		if !validConnectorName(name) {
			return nil, fmt.Errorf("@%s(%q): %s", MirroredToAnnotation, name, connectorNameFormHint(name))
		}
		if _, dup := seen[name]; dup {
			// One entry per target, because the outbox appends ONE entry
			// per target per write (D5). A repeated name would double
			// every delivery, and the idempotency key -- (concept, row,
			// version, target) -- is identical for both, so the second
			// would be silently swallowed rather than loudly refused.
			return nil, fmt.Errorf("@%s names %q twice -- one entry per mirror target",
				MirroredToAnnotation, name)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// connectorNameFormHint renders why a name is malformed, naming the
// rule rather than restating the regex.
func connectorNameFormHint(name string) string {
	return fmt.Sprintf(
		"a connector name is a lowerCamelCase identifier (letters and digits, starting lowercase), e.g. %q or %q -- %q is not",
		"shopify", "quickBooks", name)
}

// ValidateOriginDecl checks the half of a declaration that needs BOTH
// annotations in hand: @mirroredTo on a concept whose origin is
// external.
//
// That pairing is refused rather than accepted-and-ignored because it
// asks for re-mirroring -- MemQL forwarding somebody else's data onward
// to a third system -- which is the ORIGIN's job and not MemQL's. A
// mirror is a faithful copy; a faithful copy that also publishes is a
// second origin wearing the first one's badge, and the badge is the
// whole point (D3).
func ValidateOriginDecl(d OriginDecl) error {
	if d.IsMemQLOrigin() || len(d.MirroredTo) == 0 {
		return nil
	}
	return fmt.Errorf(
		"@%s is not valid on a concept whose @%s is %q: this concept is a MIRROR of %s, and re-mirroring it onward is %s's job, not MemQL's. Drop @%s, or move the origin to MemQL (@%s(%q)) if MemQL is meant to own this data",
		MirroredToAnnotation, OriginAnnotation, d.Origin, d.Origin, d.Origin,
		MirroredToAnnotation, OriginAnnotation, OriginMemQL)
}

// FormatOrigin renders an origin declaration in its canonical
// spelling, or "" when the concept takes the MemQL default and declares
// no mirror targets (where the annotation carries no information).
func FormatOrigin(d OriginDecl) string {
	if d.IsMemQLOrigin() && len(d.MirroredTo) == 0 {
		return ""
	}
	if !d.IsMemQLOrigin() {
		return fmt.Sprintf("@%s(%q)", OriginAnnotation, d.Origin)
	}
	return fmt.Sprintf("@%s(%q)", OriginAnnotation, OriginMemQL)
}

// FormatMirroredTo renders the mirror-target declaration, or "" when
// there are none.
func FormatMirroredTo(d OriginDecl) string {
	if len(d.MirroredTo) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(d.MirroredTo))
	for _, n := range d.MirroredTo {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	return fmt.Sprintf("@%s(%s)", MirroredToAnnotation, strings.Join(quoted, ", "))
}
