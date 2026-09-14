package dslconformance

// A CALLER GATE THAT LIVES IN THE HEAD (epic memql#5166).
//
// Every authorization gate this package knew was a filter LEAF -- a spec named
// as a top-level conjunct, or an equality against an actor field -- and each of
// its detectors reads a construct's filter to decide whether the construct is
// caller-scoped.
//
// `@requiresRank("<slug>")` and `@requiresCapability("<verb>", "<resource>")`
// are caller gates too, and they sit ABOVE the signature. To a detector that
// reads only the filter, a construct migrating from a spec conjunct to one of
// these looks like a gate that simply vanished -- which is what all four gates
// reported the moment the nine constructs moved. That reading is exactly
// backwards: an annotation is checked at LOAD (a typo refuses boot) and
// enforced at execution on the direct call AND on every plan that expands the
// construct, while a spec conjunct is checked by nothing and can be silently
// dropped.
//
// So the detectors ask this as well. It is deliberately a separate question
// from the filter one rather than folded into `clauseGuarantees`: an annotation
// has no polarity and no position to get wrong, and pretending it is a leaf
// would mean a filter-shaped rule ("must be a top-level conjunct") reasoning
// about text that is not in the filter.

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// annotationGateSigRe finds a construct's signature line.
var annotationGateSigRe = regexp.MustCompile(`(?m)^(?:query|mutation|logic)\s+\w+\s+%s\s*\{`)

// carriesAnnotationGate reports whether the named construct in `src` declares
// `@requiresRank` or `@requiresCapability`.
//
// It walks back from the signature only as far as the PREVIOUS construct's
// closing brace, so an annotation belonging to the construct above cannot be
// read as this one's -- the same discipline the filter detectors get for free
// by taking one construct's body.
//
// A construct this cannot find answers FALSE, which is the fail-closed
// direction here: the caller then reports the construct as ungated, which is a
// visible failure somebody fixes, rather than clearing a gate on a construct
// the walk could not locate.
func carriesAnnotationGate(src, name string) bool {
	sig := regexp.MustCompile(strings.Replace(annotationGateSigRe.String(), "%s", regexp.QuoteMeta(name), 1))
	loc := sig.FindStringIndex(src)
	if loc == nil {
		return false
	}
	head := src[:loc[0]]
	if end := strings.LastIndex(head, "\n}\n"); end >= 0 {
		head = head[end:]
	}
	return dslgate.ConstructCarriesActorGate(head)
}
