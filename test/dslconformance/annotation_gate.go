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

	"github.com/znasllc-io/memql/component/language/dslclause"
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
	return dslgate.ConstructCarriesActorGate(constructHead(src, loc[0]))
}

// braceLessDeclRe matches the opening line of an edition-2026 brace-less
// declaration: `spec <binding> <name> = ...` or `trait <name> = ...`.
var braceLessDeclRe = regexp.MustCompile(`(?m)^(?:spec|trait)[ \t]+\w+(?:[ \t]+\w+)?[ \t]*=`)

// constructHead returns the text between the end of the construct ABOVE the
// signature that starts at sigStart and that signature -- the annotation
// block, and nothing that belongs to another construct.
//
// The end of the construct above is its closing brace, or, for an
// edition-2026 brace-less spec or trait (epic memql#5363), the last line of
// its expression (dslclause.ClauseExtent). A brace-less declaration has no
// brace to stop at, so a walk back to the previous `}` crossed it and read
// the spec's own annotations -- and whatever sat above it -- as the head of
// the construct below.
func constructHead(src string, sigStart int) string {
	head := src[:sigStart]
	if end := strings.LastIndex(head, "\n}\n"); end >= 0 {
		head = head[end:]
	}
	code := dslgate.BlankComments(head)
	locs := braceLessDeclRe.FindAllStringIndex(code, -1)
	if len(locs) == 0 {
		return head
	}
	decl := locs[len(locs)-1][0]
	lines := strings.Split(code[decl:], "\n")
	off := decl
	for i, last := 0, dslclause.ClauseExtent(lines, 0); i <= last; i++ {
		off += len(lines[i]) + 1
	}
	if off > len(head) {
		return ""
	}
	return head[off:]
}
