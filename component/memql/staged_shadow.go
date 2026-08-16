package memql

// STAGED-DATA SHADOW MODE (memql#3981, phase 2 of epic memql#3974).
//
// Epic #3974 gives rows written against a not-yet-trained concept somewhere
// to be: they are STAGED, invisible on the ordinary read path, and training
// the concept makes them live. Before enforcing that, measure what enforcing
// it WOULD do.
//
// THIS FILE INJECTS NOTHING. No query returns a different row with it
// landed, no plan is rewritten, no read seam is touched, and nothing here
// runs at query time at all -- the only caller is a test. The output is
// evidence for the enforcement ruling the later phases are taken against,
// and evidence is worthless if producing it changed the thing measured.
//
// It is deliberately modelled on rowauthz_shadow.go, whose phase-2
// measurement produced the evidence memql#3172's enforcement ruling was
// taken against. Two of that file's design constraints carry over verbatim:
//
//   - The analyzer is PURE -- construct in, verdict out. No database, no
//     traffic, no engine state, no context.Context. That is what lets the
//     tree-wide measurement run as an ordinary test over the loaded
//     registry, and it is also the #3976 audience ruling holding: a staged
//     row is visible to NOBODY on the ordinary path, so the predicate is a
//     CONSTANT, so there is no caller for an analyzer to need. If a later
//     change finds itself wanting a context here, that is the audience
//     ruling being reversed and it belongs on the issue, not in this file.
//   - `undecidable` is a FIRST-CLASS answer. Where the effect cannot be
//     decided statically the analyzer says so instead of guessing in either
//     direction. An overstated blast radius misleads a ruling exactly as
//     badly as an understated one.
//
// ---------------------------------------------------------------------
// WHAT THIS ANALYZER ASKS, AND WHY IT IS NOT THE ROW-AUTHZ QUESTION
// ---------------------------------------------------------------------
//
// rowauthz_shadow.go asks: IS THE PREDICATE IMPLIED by what the author
// already wrote? That question exists because a row-authz predicate is a
// function of the CALLER (`ownerUserId == actor.userId`), so an author can
// have written it by hand and enforcement would then be a no-op.
//
// Staged visibility has no such shape, and two rulings are why:
//
//   - #3977 measured the marking model and chose CONCEPT-GRAIN: no row
//     marker on MemoryNodes at all. Visibility is a pure function of the
//     concept, and every row carries its concept.
//   - #3976 ruled the audience is NOBODY, with the staged scope set by the
//     CALLER on the read rather than inferred from identity. So the
//     predicate is a constant.
//
// A constant predicate over a column every row carries is not something an
// author can have half-written. There is nothing for implication to turn
// on. What is left to decide statically is whether a statically-placed
// predicate can REACH the read at all -- so this analyzer asks:
//
//	IS THE PREDICATE PLACEABLE HERE?
//
// ---------------------------------------------------------------------
// THE DIRECTION OF CAUTION INVERTS, AND THAT IS THE POINT
// ---------------------------------------------------------------------
//
// In row-authz, `would-narrow` is the alarming verdict: rows a caller sees
// today stop coming back, so each entry is either a latent authorization
// bug or a legitimate broad read needing an explicit declaration.
//
// Here `would-hide` is the DESIGNED behaviour. Hiding a staged concept's
// rows from the ordinary read path is what staging MEANS; a construct
// reported would-hide is a construct the mechanism works on.
//
// The alarming verdict here is `undecidable`, because an undecidable read
// is a read the predicate cannot be placed on -- which under staging is a
// staged row escaping onto the ordinary path. So this analyzer must never
// trade an undecidable for a confident answer to keep the numbers tidy,
// and it must not manufacture undecidables either: an inflated undecidable
// bucket drowns the constructs that genuinely are one, which is the whole
// finding.
//
// Concretely, this is why an opaque conjunct does NOT push a bound
// construct into `undecidable` the way it does in AnalyzeShadow. There,
// an unexpanded spec might itself carry the tier's term, so concluding
// "not implied" without expanding it would report a change enforcement
// might not make. Here the predicate is ANDed at the root and names the
// bound concept, so with that concept staged EVERY row of it leaves the
// result set no matter what the rest of the filter says. Whether the
// filter had already excluded them is a question about what the construct
// returned before, not about whether the predicate covers the read.
//
// ---------------------------------------------------------------------
// NAMING: "staged DATA", not "staged CONSTRUCT"
// ---------------------------------------------------------------------
//
// Two live epics use the word `staged` for different things, and a reader
// who conflates them will misread every verdict in this file:
//
//   - memql#3928 (authoring_staged.go, shipped) -- a staged CONSTRUCT is a
//     durable authored construct that resolves for its AUTHOR alone until
//     it is trained. `ConstructStaged` is a v1:authoring:construct.status
//     value; the tier is about WHO CAN CALL a construct.
//   - memql#3974 (this file) -- staged DATA is rows written against a
//     not-yet-trained CONCEPT. The tier is about WHICH ROWS COME BACK.
//
// Everything exported here therefore carries the `StagedData` prefix. It
// is longer than `Staged*` and that is the trade: the shorter name would
// sit in the same package as the #3928 tier and read as its sibling.

import (
	"fmt"
	"strings"
)

// StagedDataVerdict is what staged-data shadow mode concluded about one
// construct.
type StagedDataVerdict string

const (
	// StagedDataWouldHide -- the construct DECLARES the concept it reads,
	// so the predicate has a statically-determined place, and with that
	// concept staged every row of it leaves this construct's result set.
	//
	// The name is `would-hide` rather than `would-narrow` because under
	// concept-grain the narrowing is TOTAL for the named concept: the
	// injected term contradicts the bound-concept equality the loader
	// guarantees (ensureBoundConceptFilter, function_loader.go), so the
	// construct returns none of that concept's rows rather than fewer of
	// them. `would-narrow` would understate a total effect, and this
	// file's doctrine forbids understatement exactly as loudly as
	// overstatement.
	StagedDataWouldHide StagedDataVerdict = "would-hide"

	// StagedDataNoChange -- the predicate is placeable AND leaves this
	// construct's result set exactly as it is.
	//
	// Two ways to get here, and they are one verdict because the answer a
	// reader acts on is the same: nothing changes. Either the hypothesis
	// does not stage this construct's concept (a statement about the
	// hypothesis, the way AnalyzeShadow's `public` tier is a statement
	// about the tier), or the construct's own filter already excludes the
	// concept as a top-level conjunct.
	StagedDataNoChange StagedDataVerdict = "no-change"

	// StagedDataNotAReadPath -- the construct declares a concept but
	// issues no read for a READ-PATH predicate to ride.
	//
	// Kept separate from `no-change` deliberately, and the distinction is
	// the whole point of the measurement. `no-change` says the hypothesis
	// spared this read; `not-a-read-path` says there was no read here to
	// spare. Folding the two would count 261 mutations as reads the
	// predicate covers, which overstates coverage -- and coverage is the
	// only thing this measurement is for.
	StagedDataNotAReadPath StagedDataVerdict = "not-a-read-path"

	// StagedDataUndecidable -- the construct has no declared binding for a
	// statically-placed predicate to resolve from, so what the predicate
	// would do here cannot be decided by inspection.
	//
	// THE VALUABLE BUCKET. Counted separately and never folded into either
	// of the others: these are the reads a statically-placed predicate
	// cannot cover, so naming them IS the finding.
	StagedDataUndecidable StagedDataVerdict = "undecidable"
)

// StagedDataBinding is the hypothesis the analyzer is handed: the concept
// a construct declares, and whether that concept is staged.
//
// STAGEDNESS IS AN INPUT, NOT A LOOKUP, and that is a scoping decision
// rather than a convenience. The runtime accessor for a concept's staged
// state lands in memql#3980; depending on it would make this analyzer a
// reader of live lifecycle state and put the measurement at the mercy of
// whatever the tree happens to have staged on the day it runs. The
// question here is about the CONSTRUCT -- "what would happen if the
// concept you bind were staged" -- which has a stable answer with no
// runtime anywhere in it.
//
// Concept is resolved from the construct's DECLARED BINDING
// (Function.BoundConcept), never from filter text. That is the same source
// enforcement resolves from (enforceRowAuthzOnPlan reads plan.BoundConcept
// and has no filter-text fallback, deliberately) and the same source
// memql#3172 finding 1 forced: extractConceptFromExpression answers "" for
// anything that is not a top-level `concept==<id>` equality, so a measurement
// reading filter text would go quiet on exactly the spellings the seam covers.
type StagedDataBinding struct {
	// Concept is the construct's declared binding, or "" when it has none.
	Concept string
	// Staged is the hypothesis: is that concept staged?
	Staged bool
}

// StagedDataShadowRecord is one measurement: what would happen to one
// construct if the concept it binds were staged.
type StagedDataShadowRecord struct {
	Construct string `json:"construct"`
	// Kind is the construct kind as the loader stamped it
	// (query / mutation / logic / builtin).
	Kind    string `json:"kind"`
	Concept string `json:"concept,omitempty"`
	// Predicate is the term that would be ANDed in, rendered the way an
	// author would have written it. Rendered, never applied.
	Predicate string            `json:"predicate,omitempty"`
	Verdict   StagedDataVerdict `json:"verdict"`
	// Reason explains an undecidable or not-a-read-path verdict in the
	// terms a reader needs to act on it.
	Reason string `json:"reason,omitempty"`
	// Origin is the source file the construct was loaded from, so an entry
	// can be found without grepping the whole tree for its name.
	Origin string `json:"origin,omitempty"`
	// Traverses records that the construct also reaches rows by GRAPH
	// EXPANSION. Not a verdict and deliberately not folded into one: the
	// predicate's effect on the BOUND concept is unchanged, but graph
	// expansion "has no filter to AND anything into" (rowauthz_enforce.go),
	// so for the concepts a traversal lands on the injection seam is not a
	// weaker mechanism, it is no mechanism. Reported alongside the verdict
	// rather than inside it because the two facts are independent and a
	// reader needs both.
	Traverses bool `json:"traverses,omitempty"`
}

// StagedDataPredicate renders the term a staged concept would AND into
// every ordinary read of it, in the spelling an author would have written
// by hand. It is rendered, never applied.
//
// TWO THINGS ABOUT THE SPELLING ARE LOAD-BEARING.
//
// It is `row.concept`, not a bare `concept`. A filter mixes two field
// surfaces under one syntax -- payload properties are bare and row
// intrinsics live under the `row.` namespace -- and the bare spelling is
// RETIRED, rejected by TestFilterIntrinsicsUseRowNamespace (memql#2779).
// This is the identical trap InjectedPredicate documents for `row.id`:
// concatenating the intrinsic blindly emits something that reads as a
// payload property and compiles to an entirely different SQL path, a
// silently wrong predicate, which is the one outcome a renderer must never
// produce.
//
// It carries NO reference to `actor` or any other caller value, and that
// is the #3976 audience ruling made checkable rather than merely written
// down: a staged row is visible to nobody on the ordinary read path, so
// the predicate is a CONSTANT. TestStagedDataPredicateIsCallerIndependent
// fails if a future edit puts an identity back into it -- which is what
// reversing that ruling would look like from here, and it would carry with
// it the requirement to thread an actor through every read seam including
// the ~55 hand-rolled SQL sites of memql#3984.
//
// Returns "" when there is no binding or the hypothesis does not stage it,
// mirroring InjectedPredicate's "" for the public tier: no predicate,
// explicitly, rather than a predicate that happens to match everything.
func StagedDataPredicate(binding *StagedDataBinding) string {
	if binding == nil || !binding.Staged {
		return ""
	}
	concept := strings.TrimSpace(binding.Concept)
	if concept == "" {
		return ""
	}
	return fmt.Sprintf("row.concept!=%q", concept)
}

// AnalyzeStagedDataShadow decides what a staged-visibility predicate would
// do to one construct.
//
// kind is the construct kind the loader stamped (Function.FunctionKind);
// expr is its loaded expression, which for a construct that issues no read
// is nil; binding is the hypothesis. All three are data off the loaded
// construct -- there is no fourth argument and there is deliberately no
// context.Context (see the file header).
//
// THE ORDER OF THE ARMS IS THE ARGUMENT, so it is spelled out here rather
// than left to be reverse-engineered from the switch:
//
//  1. NO BINDING comes first, so it catches a builtin as well as a logic.
//     A builtin issues no read EXPRESSION but very much reads -- its Go
//     executor runs its own SQL, which is the population memql#3984
//     inventories -- so classifying it by "has no expression" would file
//     it under not-a-read-path and hide the exact case this bucket
//     exists to name.
//  2. NOT-A-READ-PATH second, over what is left: a construct that
//     declares a concept and still issues no read expression. In this tree
//     that is the mutations, and it is only reachable after arm 1 has
//     taken the unbound constructs away.
//  3. The hypothesis, then the filter, then would-hide.
func AnalyzeStagedDataShadow(kind string, expr ExpressionNode, binding *StagedDataBinding) (StagedDataVerdict, string) {
	concept := ""
	if binding != nil {
		concept = strings.TrimSpace(binding.Concept)
	}

	// ARM 1 -- no declared binding.
	if concept == "" {
		return StagedDataUndecidable, stagedDataUnboundReason(kind, expr)
	}

	// ARM 2 -- declared, but nothing on the read path to inject into.
	if expr == nil {
		return StagedDataNotAReadPath, stagedDataNoReadReason(kind, concept)
	}

	// ARM 3 -- the hypothesis leaves this concept alone.
	if !binding.Staged {
		return StagedDataNoChange, fmt.Sprintf("the hypothesis does not stage %s, so nothing is injected", concept)
	}

	// ARM 4 -- the filter already excludes the concept.
	//
	// A loaded query's Expr is the whole read pipeline --
	// shape(paginate(sort(<filter>))) -- not the filter, so the wrappers
	// come off first. unwrapToFilter is reused from rowauthz_shadow.go
	// rather than copied: peeling read-pipeline directives is a property of
	// the expression language, not of row-authz, and its CountExpression
	// reasoning holds here unchanged (applyDirectiveWrappers peels the
	// count onto plan.Count BEFORE anything is injected into plan.Root, so
	// the analyzer must look at the same expression the injector does).
	// Two copies of a peeler is how two detectors drift into disagreeing
	// about what they are both looking at.
	filter := unwrapToFilter(expr)
	if conjuncts, ok := topLevelConjunctsOf(filter); ok {
		for _, c := range conjuncts {
			if stagedDataExcludesConcept(c, concept) {
				return StagedDataNoChange, fmt.Sprintf(
					"a top-level conjunct already excludes %s, so the injected term removes nothing this read returns", concept)
			}
		}
	}

	// ARM 5 -- would-hide.
	//
	// No opacity check, and its ABSENCE is deliberate: see the file header.
	// The injected term is ANDed at the root and names the bound concept,
	// so with that concept staged every row of it leaves the result set
	// whatever an unexpanded spec conjunct may also have been doing.
	return StagedDataWouldHide, fmt.Sprintf(
		"the predicate resolves from the declared binding and %q removes every row of the concept from this read",
		StagedDataPredicate(binding))
}

// stagedDataUnboundReason names WHY a construct offers no binding, from
// the construct alone.
//
// This is where the undecidable bucket earns its keep. "Undecidable" on
// its own tells a reader nothing actionable; the shape of the expression
// says which of several genuinely different holes they are looking at, and
// the report groups on exactly these strings. The default arm is an
// ALLOW-LIST by construction, matching firstOpaqueConjunct's posture:
// anything not positively recognised is named by its type rather than
// silently folded into a neighbouring case.
func stagedDataUnboundReason(kind string, expr ExpressionNode) string {
	if expr == nil {
		if strings.EqualFold(strings.TrimSpace(kind), FunctionTypeBuiltin) {
			return "builtin: the read runs inside a Go executor, so it reaches storage without passing a plan at all -- " +
				"neither the injection seam nor the row gate sees it (the population memql#3984 inventories)"
		}
		return "the construct carries neither a declared binding nor a read expression"
	}
	switch n := unwrapToFilter(expr).(type) {
	case *BuiltinFunctionExpression:
		// The seam gap memql#3982 names, seen from the construct side: at
		// plan.Root a bare builtin call short-circuits executeWith before
		// the query path runs, so it is invisible to the injection seam
		// (which resolves from plan.BoundConcept, and a logic binds none)
		// AND to the row gate (which lives in evaluateExpressionSet).
		//
		// This arm fires on a PLAN root, not on a registry entry: the
		// loader stores a logic's `return <builtin>({...})` as a
		// FunctionCallExpression and the validator rewrites it to this node
		// only when the call is resolved. Both forms reach this function
		// and both must be named, or the analyzer would answer one of them
		// and be silent on the other.
		return fmt.Sprintf("the read expression is a top-level builtin call %q; at plan.Root this is the seam gap memql#3982 names", n.Name)
	case *FunctionCallExpression:
		// Deliberately NOT expanded. Resolving the callee needs the
		// function registry, which would make the analyzer a reader of
		// loaded state and give it a second way to answer a question the
		// seam answers from the binding alone. AnalyzeShadow refuses to
		// expand a spec reference for the same reason and calls the
		// refusal an answer.
		//
		// The callee's NAME is carried in the reason rather than swallowed
		// precisely so a caller that legitimately holds the registry -- the
		// report, which is not the analyzer -- can classify the entry
		// without this function growing a lookup.
		return fmt.Sprintf("the read expression is a call to %q, which this analyzer does not expand", n.Name)
	case *SpecReferenceExpression:
		return fmt.Sprintf("the read expression is the spec %q, which this analyzer does not expand", n.Name)
	case *CollectionMethodExpression:
		return fmt.Sprintf("the read expression is the collection method %q over a %T", n.Method, n.Receiver)
	case *RelationshipExpression:
		return "the read expression is a relationship traversal, which reaches rows through relationship definitions and has no filter to AND anything into"
	default:
		return fmt.Sprintf("the read expression is a %T and the construct declares no binding", n)
	}
}

// stagedDataNoReadReason explains a not-a-read-path verdict, and names the
// one read such a construct DOES perform so the verdict cannot be misread
// as "this construct never touches a row".
func stagedDataNoReadReason(kind, concept string) string {
	if strings.EqualFold(strings.TrimSpace(kind), "mutation") {
		// Precise, because the loose version would be wrong. A mutation
		// does read: executeWrite's read-merge calls loadPriorPayload,
		// which goes to conceptMeta.Query over the store directly and
		// never builds a QueryPlan -- so no injection seam exists on it to
		// place a predicate into. Under this epic that is also the CORRECT
		// behaviour rather than a gap: writing to a staged concept is the
		// premise, and a staged write must be able to read-merge its own
		// staged prior row. The write side is its own task.
		return "a write: its read-merge reaches the prior row through loadPriorPayload -> conceptMeta.Query, which builds no plan, " +
			"so the read-path predicate has nowhere to sit -- and under this epic a write to a staged concept is the premise, not a leak"
	}
	return fmt.Sprintf("the construct binds %s but carries no read expression for a read-path predicate to ride", concept)
}

// stagedDataExcludesConcept reports whether one top-level conjunct already
// keeps the named concept's rows out.
//
// Narrow on purpose. Two operators can express the exclusion -- `!=` and
// `not in` -- and nothing else is credited: this arm exists so a filter
// that has genuinely already done the work is not reported as changed, and
// a matcher looser than that would credit an exclusion the predicate does
// not actually duplicate. The engine AST has no NOT node (ast_converter.go
// rejects it outright and points at the `!=` form), so negation always
// arrives on the operator, which is why matching two operators is
// exhaustive rather than a sample.
func stagedDataExcludesConcept(node ExpressionNode, concept string) bool {
	cmp, ok := node.(*ComparisonExpression)
	if !ok || cmp == nil || !stagedDataIsConceptField(cmp.Field) {
		return false
	}
	switch cmp.Operator {
	case OpNe:
		v, isString := cmp.Value.(string)
		return isString && strings.TrimSpace(v) == concept
	case OpOut:
		for _, item := range stagedDataStringList(cmp.Value) {
			if item == concept {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// stagedDataIsConceptField reports whether a field reference names the
// `concept` row intrinsic.
//
// BOTH SPELLINGS ARE ACCEPTED, and it is not laxity. The AUTHORED form is
// `row.concept` (memql#2779's namespace rule, which is also what
// StagedDataPredicate renders), while the form the loader builds is the
// bare single part -- ensureBoundConceptFilter constructs
// `FieldReference{Raw: "concept", Parts: []string{"concept"}}` and
// extractConceptFromExpression only ever looks at `len(Parts) == 1`. A
// matcher that took one spelling would miss the other half of the tree it
// is measuring, which is the near-miss isRowIdName documents for `row.id`.
func stagedDataIsConceptField(f FieldReference) bool {
	switch len(f.Parts) {
	case 0:
		return strings.EqualFold(strings.TrimSpace(f.Raw), "concept")
	case 1:
		return strings.EqualFold(strings.TrimSpace(f.Parts[0]), "concept")
	case 2:
		return strings.EqualFold(strings.TrimSpace(f.Parts[0]), "row") &&
			strings.EqualFold(strings.TrimSpace(f.Parts[1]), "concept")
	default:
		return false
	}
}

// stagedDataStringList flattens the value side of an `in` / `not in`
// comparison to the strings it names, ignoring anything that is not one.
func stagedDataStringList(v any) []string {
	switch t := v.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			out = append(out, strings.TrimSpace(s))
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

// StagedDataTraverses reports whether a construct reaches rows by GRAPH
// EXPANSION -- a relationship traversal, or a `withDepth` directive.
//
// Recorded next to the verdict rather than inside it. The verdict answers
// what the predicate does to the construct's BOUND concept, which a
// traversal does not change; this answers whether the construct also
// reaches rows of concepts the binding never names, for which the
// injection seam is not a weaker mechanism but no mechanism at all
// (rowauthz_enforce.go: graph expansion "has no filter to AND anything
// into"). Two independent facts, so two fields -- folding them would make
// a reader unable to recover either.
//
// It walks the WHOLE expression including the directive wrappers, since
// DepthExpression is one of the wrappers unwrapToFilter peels off.
func StagedDataTraverses(expr ExpressionNode) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *RelationshipExpression:
		return true
	case *DepthExpression:
		return true
	case *ShapeExpression:
		return StagedDataTraverses(n.Target)
	case *PaginateExpression:
		return StagedDataTraverses(n.Target)
	case *SortExpression:
		return StagedDataTraverses(n.Target)
	case *SelectExpression:
		return StagedDataTraverses(n.Target)
	case *TimestampExpression:
		return StagedDataTraverses(n.Target)
	case *CountExpression:
		return StagedDataTraverses(n.Target)
	case *LogicalExpression:
		return StagedDataTraverses(n.Left) || StagedDataTraverses(n.Right)
	default:
		return false
	}
}
