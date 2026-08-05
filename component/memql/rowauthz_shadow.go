package memql

// Row-authz SHADOW MODE (memql#2921, Phase 2 of the #2803 ruling).
//
// Phase 1 (#2920) made each concept DECLARE who may see its rows. This
// phase computes the predicate that declaration WOULD inject, decides
// whether the author's filter already implies it, and records the
// verdict. It injects nothing. No query returns a different row than
// it did before, with the gate on or off.
//
// The output is the evidence #2803's Phase 3+ enforcement ruling is
// taken against: a `would-narrow` list by construct name, where each
// entry is either a latent authorization bug being caught or a
// legitimate admin-tier read that needs an explicit declaration.
// Which of the two it is cannot be read off a count, so the LIST is
// the deliverable and the number is a summary of it.
//
// Two design constraints run through this file:
//
//   - The analyzer is PURE -- expression plus declared tier in, verdict
//     out. No database, no traffic, no engine state. That is what lets
//     the tree-wide measurement run as an ordinary test over the loaded
//     registry, and it means the runtime hook and the report cannot
//     disagree about a verdict, the same one-detector constraint Phase 1
//     was built under.
//   - `undecidable` is a first-class answer. Where implication cannot be
//     decided statically the analyzer says so instead of guessing, in
//     either direction. An overstated blast radius misleads a ruling
//     exactly as badly as an understated one.

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// ShadowVerdict is what shadow mode concluded about one access.
type ShadowVerdict string

const (
	// ShadowAlreadyImplied -- the author's filter already guarantees the
	// injected predicate, so enforcement would not change this result
	// set. Also the verdict for the `public` tier, which injects
	// nothing.
	ShadowAlreadyImplied ShadowVerdict = "already-implied"
	// ShadowWouldNarrow -- the injected predicate is not implied, so
	// enforcement would remove rows this access returns today.
	ShadowWouldNarrow ShadowVerdict = "would-narrow"
	// ShadowUndecidable -- implication cannot be decided statically.
	// Counted separately and never folded into either of the others:
	// these are the accesses enforcement would change blindly.
	ShadowUndecidable ShadowVerdict = "undecidable"
)

// ShadowRecord is one measurement: what would happen to one access if
// the concept's declared tier were enforced.
type ShadowRecord struct {
	// Construct is the named query the access came from, or a synthetic
	// label for a path that has no construct name (inline DSL, graph
	// expansion).
	Construct string `json:"construct"`
	Concept   string `json:"concept"`
	Tier      string `json:"tier"`
	// Predicate is the term that would be ANDed in, rendered the way an
	// author would have written it.
	Predicate string        `json:"predicate"`
	Verdict   ShadowVerdict `json:"verdict"`
	// Reason explains a would-narrow or undecidable verdict in the terms
	// a reader needs to act on it.
	Reason string `json:"reason,omitempty"`
	// Path names the read path this record came from, so a measurement
	// can show that graph expansion was covered rather than assert it.
	Path string `json:"path"`
}

// Read paths, so the report can show coverage instead of claiming it.
const (
	ShadowPathFilter         = "filter"
	ShadowPathGraphExpansion = "graph-expansion"
)

// InjectedPredicate renders the term a tier would AND into every access
// of the concept, in the spelling an author would have written by hand.
// It is rendered, never applied.
func InjectedPredicate(decl *langparser.RowAuthzDecl) string {
	if decl == nil {
		return ""
	}
	switch decl.Tier {
	case langparser.RowAuthzOwned:
		// The SELF-OWNED form names the row's own identity, not a payload
		// field (memql#3029), so it renders `row.id` rather than a bare `id`.
		// A filter spells payload properties bare and row intrinsics under the
		// `row.` namespace; the bare spelling is RETIRED and rejected by
		// TestFilterIntrinsicsUseRowNamespace (memql#2779). Concatenating the
		// owner blindly would emit `id==actor.userId`, which reads as a
		// payload property named `id` and compiles to an entirely different
		// SQL path -- a silently wrong predicate, which is the one outcome
		// this renderer must never produce.
		if decl.Owner == langparser.RowAuthzSelfOwnedField {
			return "row.id==actor.userId"
		}
		return decl.Owner + "==actor.userId"
	case langparser.RowAuthzClusterOwner:
		return "actor.isClusterOwner==true"
	case langparser.RowAuthzPublic:
		return "" // public injects nothing, explicitly
	case langparser.RowAuthzGranted:
		return decl.Spec
	default:
		return ""
	}
}

// AnalyzeShadow decides what enforcing decl would do to an access whose
// author-written filter is expr.
//
// expr must be the expression BEFORE resolveActorReferences runs. That
// function substitutes `actor.userId` with the caller's concrete id, so
// analysing the resolved form would see `ownerUserId=="u_123"` where the
// author wrote `ownerUserId==actor.userId` and report `would-narrow` for
// every construct that already carries the term -- inverting the whole
// measurement.
//
// A nil expr means the access has no filter at all (graph expansion,
// an unfiltered read), which cannot imply a narrowing predicate.
func AnalyzeShadow(expr ExpressionNode, decl *langparser.RowAuthzDecl) (ShadowVerdict, string) {
	if decl == nil {
		return ShadowAlreadyImplied, ""
	}
	// A loaded query's Expr is the whole read pipeline --
	// shape(paginate(sort(<filter>))) -- not the filter. Analysing the
	// wrapper finds no conjuncts and reports would-narrow for
	// everything, which is exactly what the first run of this
	// measurement did: 33 of 33 would-narrow, including constructs
	// named `...ForOwner` that hand-write the term.
	expr = unwrapToFilter(expr)

	switch decl.Tier {
	case langparser.RowAuthzPublic:
		// Declared globally readable: no predicate, so nothing can
		// change. This is the one tier where already-implied is a
		// statement about the tier rather than about the filter.
		return ShadowAlreadyImplied, ""

	case langparser.RowAuthzGranted:
		// The predicate is a relationship spec whose implication by an
		// arbitrary filter is not decidable by inspection -- it needs
		// the join semantics Phase 4 is scoped to build. The one case
		// that IS decidable is the filter naming the same spec as a
		// top-level conjunct.
		conjuncts, ok := topLevelConjunctsOf(expr)
		if !ok {
			return ShadowUndecidable, "filter is a top-level disjunction, so no term is guaranteed"
		}
		for _, c := range conjuncts {
			if ref, isSpec := c.(*SpecReferenceExpression); isSpec && ref.Name == decl.Spec {
				return ShadowAlreadyImplied, ""
			}
		}
		return ShadowUndecidable, fmt.Sprintf(
			"granted tier: implication of spec %q by this filter needs relationship semantics that do not exist yet", decl.Spec)

	case langparser.RowAuthzOwned, langparser.RowAuthzClusterOwner:
		// Both are decidable: the predicate is a single leaf, and a
		// filter implies it exactly when that leaf is a TOP-LEVEL
		// CONJUNCT. A term under a top-level `||` guarantees nothing --
		// the other arm still returns rows the term would exclude
		// (memql#2832).
		conjuncts, ok := topLevelConjunctsOf(expr)
		if !ok {
			// A top-level disjunction. `(owner && a) || (owner && b)`
			// DOES imply the predicate even though no single top-level
			// conjunct exists, so reporting would-narrow here would be a
			// confident wrong answer -- and this file's doctrine is that
			// an overstated blast radius misleads a ruling as badly as an
			// understated one. Every disjunct implying it is implied;
			// anything else is undecidable, not narrowed.
			if allDisjunctsImply(expr, decl) {
				return ShadowAlreadyImplied, ""
			}
			return ShadowUndecidable, "filter is a top-level disjunction and not every arm guarantees the tier's term"
		}
		for _, c := range conjuncts {
			switch {
			case decl.Tier == langparser.RowAuthzOwned && isOwnerScopeLeaf(c, decl.Owner):
				return ShadowAlreadyImplied, ""
			case decl.Tier == langparser.RowAuthzClusterOwner && isClusterOwnerLeaf(c):
				return ShadowAlreadyImplied, ""
			}
		}
		// Before concluding would-narrow, check whether anything in the
		// filter is opaque to this analyzer. A spec reference that could
		// not be expanded might itself carry the term.
		if opaque := firstOpaqueConjunct(conjuncts); opaque != "" {
			return ShadowUndecidable, fmt.Sprintf(
				"filter contains %s, which this analyzer cannot expand, so implication is not decidable", opaque)
		}
		if expr == nil {
			return ShadowWouldNarrow, "access has no filter, so it reads every row of the concept"
		}
		return ShadowWouldNarrow, fmt.Sprintf(
			"no top-level conjunct guarantees %q", InjectedPredicate(decl))

	default:
		return ShadowUndecidable, fmt.Sprintf("unknown tier %q", decl.Tier)
	}
}

// unwrapToFilter peels the read-pipeline wrappers off an expression to
// reach the filter underneath.
//
// A loaded query construct's `Expr` is the full pipeline the struct
// form compiles to -- `shape(paginate(sort(<filter>), 50), "xFull")` --
// and each of those wrappers is a single-target node. Peeling is sound
// for THIS analysis -- not because the wrappers cannot affect the
// result (paginate windows it and asOf pins it in time, both of which
// do), but because none of them can SUPPLY the tier's term. Peeling can
// therefore never manufacture an `already-implied`, which is the only
// direction that would matter. Not peeling means the analyzer never
// sees a filter at all.
//
// CountExpression is deliberately not peeled: #2803 lists aggregates as
// something row-authz does not solve, and a count over rows you cannot
// read is a different question from which rows a filter selects.
// Leaving it wrapped makes it opaque, so it lands in `undecidable`
// rather than being silently answered.
func unwrapToFilter(expr ExpressionNode) ExpressionNode {
	for {
		switch n := expr.(type) {
		case *ShapeExpression:
			expr = n.Target
		case *PaginateExpression:
			expr = n.Target
		case *SortExpression:
			expr = n.Target
		case *SelectExpression:
			expr = n.Target
		case *TimestampExpression:
			expr = n.Target
		case *DepthExpression:
			expr = n.Target
		default:
			return expr
		}
	}
}

// topLevelConjunctsOf flattens an expression's top-level AND chain.
//
// The second return is false when the expression is a top-level
// disjunction: nothing in an OR is guaranteed, so there are no
// conjuncts to reason about rather than an empty list of them. A nil
// expression yields an empty conjunct set (there is no filter), which
// is a decidable state and not a disjunction.
func topLevelConjunctsOf(expr ExpressionNode) ([]ExpressionNode, bool) {
	if expr == nil {
		return nil, true
	}
	logical, isLogical := expr.(*LogicalExpression)
	if !isLogical {
		return []ExpressionNode{expr}, true
	}
	if logical.Op == LogicalOr {
		return nil, false
	}
	left, leftOK := topLevelConjunctsOf(logical.Left)
	if !leftOK {
		// A disjunction nested under an AND is fine -- it is simply one
		// conjunct that happens to be an OR, and it guarantees nothing
		// on its own. Keep it as an opaque conjunct rather than
		// discarding the whole chain.
		left = []ExpressionNode{logical.Left}
	}
	right, rightOK := topLevelConjunctsOf(logical.Right)
	if !rightOK {
		right = []ExpressionNode{logical.Right}
	}
	return append(left, right...), true
}

// isOwnerScopeLeaf reports whether a conjunct is `<field> == actor.userId`
// for the named field.
//
// The value side must be an unresolved *ActorReference; see AnalyzeShadow
// on why the analyzer runs before resolution.
func isOwnerScopeLeaf(node ExpressionNode, ownerField string) bool {
	cmp, ok := node.(*ComparisonExpression)
	if !ok || cmp.Operator != OpEq {
		return false
	}
	ref, isActor := cmp.Value.(*ActorReference)
	if !isActor || !strings.EqualFold(strings.TrimSpace(ref.Path), "userId") {
		return false
	}
	field := topLevelPayloadField(cmp.Field)
	if field == "" {
		return false
	}
	return strings.EqualFold(field, strings.TrimSpace(ownerField))
}

// isClusterOwnerLeaf reports whether a conjunct is the cluster-owner gate,
// written either as `actor.isClusterOwner` or `actor.isClusterOwner==true`.
func isClusterOwnerLeaf(node ExpressionNode) bool {
	cmp, ok := node.(*ComparisonExpression)
	if !ok {
		return false
	}
	if len(cmp.Field.Parts) < 2 ||
		!strings.EqualFold(strings.TrimSpace(cmp.Field.Parts[0]), "actor") ||
		!strings.EqualFold(strings.TrimSpace(cmp.Field.Parts[len(cmp.Field.Parts)-1]), "isClusterOwner") {
		return false
	}
	switch cmp.Operator {
	case OpEq:
		return isTrueLiteral(cmp.Value)
	case OpNe:
		// `actor.isClusterOwner != false` is the same gate spelled
		// inside out. Accepted because the codemod's adminLeafRe
		// tolerates the equivalent forms and the two detectors must not
		// disagree about what a cluster-owner gate looks like.
		return isFalseLiteral(cmp.Value)
	default:
		return false
	}
}

func isFalseLiteral(v any) bool {
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && !parsed
	default:
		return false
	}
}

func isTrueLiteral(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && parsed
	default:
		return false
	}
}

// topLevelPayloadField returns the TOP-LEVEL payload property a field
// reference names, or "" when the reference is a nested path.
//
// The nested case is why this is not simply "the last Part". A tier's
// predicate compares the concept's own top-level column, so
// `input.ownerUserId == actor.userId` does NOT imply
// `ownerUserId == actor.userId` -- it constrains a key inside a nested
// object that happens to share the name. Taking the last Part accepted
// it and reported `already-implied`, which under-reports the blast
// radius: the dangerous direction for a measurement that feeds an
// enforcement ruling. Nested paths are real here --
// `plansForResponsibility` filters on `input.responsibilityId`.
//
// Phase 1's codemod excludes dotted left-hand sides for the same reason
// (`ownerLeafRe`, cmd/memqlmigrate/rowauthz_infer.go), so this also
// keeps the two detectors saying the same thing.
//
// `payload.` is accepted as an explicit prefix for the top-level
// payload object, which is the one dotted spelling that does name a
// top-level property.
func topLevelPayloadField(f FieldReference) string {
	switch len(f.Parts) {
	case 0:
		return strings.TrimSpace(f.Raw)
	case 1:
		return strings.TrimSpace(f.Parts[0])
	case 2:
		if strings.EqualFold(strings.TrimSpace(f.Parts[0]), "payload") {
			return strings.TrimSpace(f.Parts[1])
		}
		return ""
	default:
		return ""
	}
}

// firstOpaqueConjunct names the first conjunct whose contribution this
// analyzer cannot determine, or "" when every conjunct is transparent.
//
// This is what keeps `would-narrow` honest. A spec reference could
// itself contain the tier's term, so concluding "not implied" without
// expanding it would report a change that enforcement might not make.
// The default arm is the load-bearing one, and it is an ALLOW-LIST by
// construction: anything this analyzer does not positively understand
// is opaque. The first run of this measurement reported 33 of 33
// would-narrow because a `ShapeExpression` wrapper fell through a
// blocklist-shaped version of this function -- a wrong verdict
// delivered with total confidence. Failing to `undecidable` on an
// unrecognised node turns that class of bug into a visible "I could not
// tell" instead of a silent overstatement of the blast radius.
func firstOpaqueConjunct(conjuncts []ExpressionNode) string {
	for _, c := range conjuncts {
		switch n := c.(type) {
		case *ComparisonExpression:
			// The only node whose contribution is fully determined by
			// inspection, and the only one the leaf matchers understand.
			continue
		case *SpecReferenceExpression:
			return fmt.Sprintf("spec %q", n.Name)
		case *RelationshipExpression:
			return "a relationship traversal"
		case *BuiltinFunctionExpression:
			return fmt.Sprintf("builtin %q", n.Name)
		case *LogicalExpression:
			if n.Op == LogicalOr {
				return "a disjunction"
			}
			return "a nested conjunction the flattener did not reach"
		default:
			return fmt.Sprintf("a %T the analyzer does not understand", c)
		}
	}
	return ""
}

// ---- runtime collection ----

// shadowCollector accumulates records for a process. It exists so a
// long-running node can be asked what it saw; the tree-wide measurement
// does not use it (that runs the pure analyzer directly).
type shadowCollector struct {
	mu      sync.Mutex
	records map[string]ShadowRecord // keyed construct+concept+path, deduped
}

var (
	shadowOnce    sync.Once
	shadowEnabled bool
	shadowStore   = &shadowCollector{records: map[string]ShadowRecord{}}
)

// ShadowEnabled reports whether shadow mode is on.
//
// OFF BY DEFAULT and gated by an env var, because this is a measurement
// tool rather than a production code path. Read once: a gate that can
// flip mid-process would make a measurement unreproducible.
func ShadowEnabled() bool {
	shadowOnce.Do(func() {
		v := strings.TrimSpace(os.Getenv("MEMQL_ROWAUTHZ_SHADOW"))
		parsed, err := strconv.ParseBool(v)
		shadowEnabled = err == nil && parsed
	})
	return shadowEnabled
}

func (c *shadowCollector) add(r ShadowRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records[r.Construct+"\x00"+r.Concept+"\x00"+r.Path+"\x00"+string(r.Verdict)+"\x00"+r.Reason] = r
}

// ShadowRecords returns everything shadow mode has observed in this
// process, sorted for a stable report.
func ShadowRecords() []ShadowRecord {
	shadowStore.mu.Lock()
	defer shadowStore.mu.Unlock()
	out := make([]ShadowRecord, 0, len(shadowStore.records))
	for _, r := range shadowStore.records {
		out = append(out, r)
	}
	sortShadowRecords(out)
	return out
}

// ResetShadowRecords clears the collector. Test-facing.
func ResetShadowRecords() {
	shadowStore.mu.Lock()
	defer shadowStore.mu.Unlock()
	shadowStore.records = map[string]ShadowRecord{}
}

func sortShadowRecords(rs []ShadowRecord) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Concept != rs[j].Concept {
			return rs[i].Concept < rs[j].Concept
		}
		if rs[i].Construct != rs[j].Construct {
			return rs[i].Construct < rs[j].Construct
		}
		return rs[i].Path < rs[j].Path
	})
}

// rowAuthzDeclFor looks up a concept's declared tier, or nil when the
// concept is unknown or undeclared. An undeclared concept is not
// measurable -- there is no predicate to compute -- which is why the
// undeclared population is reported separately rather than counted as
// already-implied.
func rowAuthzDeclFor(conceptName string) *langparser.RowAuthzDecl {
	name := strings.TrimSpace(conceptName)
	if name == "" {
		return nil
	}
	c, err := memorynodes.Get(name)
	if err != nil || c == nil {
		return nil
	}
	return c.RowAuthz
}

// recordShadow runs the analyzer for one access and stores the verdict.
// A no-op unless shadow mode is on, and it never returns an error or
// affects the caller -- a measurement must not be able to fail a query.
func recordShadow(construct, conceptName, path string, expr ExpressionNode) {
	if !ShadowEnabled() {
		return
	}
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil {
		return
	}
	verdict, reason := AnalyzeShadow(expr, decl)
	if strings.TrimSpace(construct) == "" {
		construct = "(unnamed " + path + ")"
	}
	shadowStore.add(ShadowRecord{
		Construct: construct,
		Concept:   conceptName,
		Tier:      string(decl.Tier),
		Predicate: InjectedPredicate(decl),
		Verdict:   verdict,
		Reason:    reason,
		Path:      path,
	})
}

// lookupShadowEnv reports the raw gate value and whether it was set at
// all, so a test can tell "off by default" from "off because someone
// set it to 0".
func lookupShadowEnv() (string, bool) {
	return os.LookupEnv("MEMQL_ROWAUTHZ_SHADOW")
}

// isRowIntrinsicName reports whether a field reference names a row
// intrinsic (`row.id`, `row.createdBy`, ...) rather than a payload
// property. The two live in one filter surface but compile to entirely
// different SQL -- a table column versus a JSONB path -- so a tier can
// never be hypothesised from an intrinsic.
func isRowIntrinsicName(f FieldReference) bool {
	if len(f.Parts) >= 2 && strings.EqualFold(strings.TrimSpace(f.Parts[0]), "row") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(f.Raw)) {
	case "id", "concept", "type", "createdat", "createdby", "schema":
		return true
	}
	return false
}

// allDisjunctsImply reports whether EVERY arm of a top-level
// disjunction guarantees the tier's predicate. When they all do, the
// disjunction as a whole does too, even though no single top-level
// conjunct carries it.
func allDisjunctsImply(expr ExpressionNode, decl *langparser.RowAuthzDecl) bool {
	logical, ok := expr.(*LogicalExpression)
	if !ok || logical.Op != LogicalOr {
		return false
	}
	for _, arm := range []ExpressionNode{logical.Left, logical.Right} {
		if inner, isOr := arm.(*LogicalExpression); isOr && inner.Op == LogicalOr {
			if !allDisjunctsImply(arm, decl) {
				return false
			}
			continue
		}
		verdict, _ := AnalyzeShadow(arm, decl)
		if verdict != ShadowAlreadyImplied {
			return false
		}
	}
	return true
}
