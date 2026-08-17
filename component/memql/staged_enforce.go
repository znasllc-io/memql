package memql

// staged_enforce.go -- the STAGED DATA tier becomes REAL (epic memql#3974,
// task memql#3983). memql#3980 landed the state and said, in its own file
// header, that it was inert on purpose. This is the file that consumes it.
//
// # The inversion: the ROW GATE is the correctness mechanism
//
// The brief for this epic described the work as "inject one parsed AST
// conjunct at plan.Root, plus the row gates". Under the concept-grain ruling
// the emphasis inverts, and the code is written to the inverted emphasis
// because the measurements say so:
//
//   - THE ROW GATE IS CORRECTNESS. Visibility is a pure function of the
//     CONCEPT (memql#3980 measured the alternative: a row marker costs
//     116-142ms per 10k rows and hard-errors under
//     timescaledb.enable_dml_decompression=false, or else doubles version
//     history and makes every later read 3-5x slower forever). Every row
//     carries its concept. So a gate that reads node.Concept decides staging
//     correctly with no injection at all, and it inherits the property the
//     row-authz gate beside it documents at executor.go:225-230: it is
//     "immune to how the filter was spelled: naming a row by id, a top-level
//     `||` and a negated concept all reach it identically".
//   - THE CONJUNCT IS AN OPTIMIZATION. It exists to avoid FETCHING rows the
//     gate would discard. It is a pushdown, and pushdowns are allowed to be
//     incomplete; they are not allowed to be wrong.
//
// The split is not academic. memql#3981's shadow run measured that 115 of 619
// constructs -- 82 builtin, 33 logic -- bind NO concept, so a statically
// placed predicate cannot reach 19% of the tree at all. The row gate is the
// only thing covering those, and memql#3982 (which closed the top-level
// builtin's bypass of the row gate) is what makes it trustworthy for them.
//
// # The trap: an SQL-ONLY predicate does not survive the read path
//
// This is the part that passes a green test suite and leaks in production, so
// it is spelled out.
//
// Both filtered seams return through latestMatchingNodes
// (executor_filter.go). That function reloads each scanned candidate's TRUE
// latest version through loadLatestNodes, whose query filters on `id IN (?)`
// and an optional `createdAt <= ?` and NOTHING ELSE -- no concept, no filter,
// no authz. It then does this:
//
//	candidate := node
//	if latestNode, ok := latest[id]; ok {
//	    candidate = latestNode          // the swap
//	}
//	match, err := nodeMatchesExpression(candidate, expr, payloadCache)
//
// So the SQL correctly selected a row the injected conjunct admitted, and the
// swap can overwrite it with a version the conjunct never saw. A gate proved
// only against the emitted SQL is green and leaking.
//
// TWO THINGS CLOSE IT, and both are deliberate:
//
//  1. THE PREDICATE IS EVALUABLE IN GO. nodeMatchesExpression re-runs the
//     WHOLE expression -- injected conjunct included -- against the swapped-in
//     candidate. A comparison on `concept` is handled by
//     nodeMatchesComparison's intrinsicFieldConcept arm, so the re-check
//     rejects a staged candidate on the spot. This is why the spelling below
//     is load-bearing rather than stylistic.
//  2. THE SWAP IS CO-GATED ANYWAY. latestMatchingNodes calls admitStagedRow on
//     the reloaded candidate directly. Belt and braces, because (1) depends on
//     an expression the caller supplied still containing the conjunct, and the
//     unbound-plan cases above are exactly the ones where it does not.
//
// # Why a chain of `!=` and NOT `row.concept out [...]`
//
// The `out` spelling is the one a reviewer will reach for, it compiles to a
// tidy `concept NOT IN (...)`, and it is WRONG here -- in the loudest possible
// way. nodeMatchesComparison's concept arm is:
//
//	want, err := ensureString(cmp.Value)         // a []string fails here
//	return compareStringValues(node.Concept, strings.TrimSpace(want), cmp.Operator)
//
// and compareStringValues implements OpEq and OpNe ONLY, erroring on anything
// else. So `out` compiles in SQL and HARD-ERRORS in the Go re-check -- it
// would not leak, it would break every read once any concept was staged. The
// conjunction of `!=` terms is the only spelling both halves accept, which is
// precisely the property the trap above demands.
//
// # NULL-safety
//
// The `isNotDeleted` bug (executor_filter.go, memql#1685) is the standing
// warning here: `payload.deleted != true` compiled to a plain SQL `<>`, which
// yields NULL rather than true for a row that never carried the key, so the
// predicate silently excluded every such row. An injected negative predicate
// that is not NULL-safe empties the installation.
//
// Concept-grain sidesteps that STRUCTURALLY rather than mitigating it: the
// predicate references `concept`, a column typed Go `string` and tagged
// `bun:",notnull"`, which every row has and no row can leave absent. There is
// no "every existing row carries no marker" hazard because there is no marker.
//
// The operator is tightened to IS DISTINCT FROM regardless
// (compileConceptComparison), so the guarantee rests on the comparison rather
// than on a schema annotation staying true. For real rows the two are
// identical, which is why the change is safe; it is defense in depth, not a
// fix for a live defect.
//
// # Where the conjunct is injected, and what plan.BoundConcept means here
//
// Injected at plan.Root from parseWithFunctionsAmbient, immediately after
// enforceRowAuthzOnPlan and BEFORE rewriteFilterFieldRefs -- so the term goes
// through the same `row.` stripping every authored filter does.
//
// It does NOT copy enforceRowAuthzOnPlan's early return on an empty
// plan.BoundConcept, and that is the single most important difference between
// the two injectors. Row-authz MUST resolve a tier from a bound concept
// because the tier is a per-concept declaration with nothing to bind to
// otherwise. The staged predicate binds nothing: it names the staged set
// directly, and it is exactly as meaningful on a raw query string, a
// `concept==A || concept==B` union, or an unbound expression as on a bound
// query. Requiring a binding would reproduce the hole memql#3982 just closed,
// on the same 19% of the tree.
//
// # The injection site still has no context.Context, and the scope does not
// give it one (memql#4040)
//
// parseWithFunctionsAmbient takes no ctx, and enforceRowAuthzOnPlan's
// context-freedom is recorded in three places as deliberate. Neither gained one.
//
// #3983 left this section reading "the injected conjunct is sound ONLY while the
// audience for staged rows is NOBODY", and predicted that an explicitly
// staged-scoped read would force the conjunct to become scope-aware or be
// skipped -- "because a pushdown that hides rows the gate would admit is a
// pushdown that is wrong". That was right, and memql#4040 took the first branch.
//
// It did NOT need a context to do it. The staged scope arrives as a PARAMETER,
// resolved once by executeWith and handed down -- the identical route
// auth.CallOrigin (memql#2800) and the ambient envelope (memql#3024) already
// take into this same function, both of them for the stated reason that the
// parse path is ctx-free by design and executeWith reads the context once on its
// behalf. "The injection site has no ctx" was never the constraint; "the parse
// path does not take one" was, and this codebase already had the answer to it
// twice over in the signature immediately above.
//
// THE PUSHDOWN STILL POINTS THE SAFE WAY. Both halves resolve through
// stagedConceptWithheld (staged_scope.go), so the conjunct excludes
// staged-MINUS-scoped and the gate withholds staged-MINUS-scoped. The conjunct
// therefore never hides a row the gate would admit, which is the invariant this
// file is built on, and it remains an optimization over a correctness mechanism
// rather than a second, disagreeing enforcement.
//
// stagedConceptIds is still the single place the withheld set is computed, and
// making it scope-aware is what reached every seam at once.
//
// # Staleness
//
// There is no plan cache -- parseWithFunctionsAmbient runs per Execute -- so a
// concept that transitions staged -> live is reflected on the next read rather
// than at the next restart. The RESULT cache keys off plan.Root
// (planCacheSignature), and the conjunct is part of plan.Root, so a staged and
// an unstaged read of the same query cannot share a cache entry.
//
// THAT ARGUMENT HAS AN EXCEPTION ONCE SCOPES EXIST, and it is why
// planCacheSignature carries a `stagedScope:` term (memql#4040). A scope
// covering every staged concept leaves NOTHING to exclude, so the predicate is
// empty and plan.Root is byte-identical to an ordinary caller's -- and without
// the extra term the operator's staged-inclusive result would be cached under
// that key and served to the next caller. When the
// staged set is empty -- the overwhelming common case -- nothing is injected
// and every existing signature is byte-identical to today's.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptDataStagedPrefix is the e.promotedAuthored key namespace memql#3980
// stores the staged marker under. It is duplicated here rather than derived
// because conceptDataStagedKey composes a key for ONE id and this file needs
// to ENUMERATE them, which is the opposite direction and has no shared form.
//
// TestStagedKeyPrefixMatchesTierKeyBuilder pins the two together, and that
// test earns its keep: if the tier renamed its namespace, this file's
// enumeration would quietly find NOTHING, report an empty staged set, inject
// no conjunct and admit every row. The failure mode of a drifted prefix is
// FAIL-OPEN -- staged data published -- which is the one class of breakage
// that cannot be allowed to be silent.
const conceptDataStagedPrefix = "conceptDataStaged:"

// stagedPredicateCache memoizes the PARSED predicate per staged-set
// signature, mirroring rowAuthzPredicateCache. The signature is the sorted,
// newline-joined concept id list, so it changes exactly when the staged set
// does. Values are handed out through cloneStagedPredicate -- never shared --
// because rewriteFilterFieldRefs mutates the nodes it walks in place.
var stagedPredicateCache sync.Map

// stagedConceptIds returns the canonical ids of every concept whose DATA is
// WITHHELD FROM THIS READ, sorted so the derived signature is stable across
// calls.
//
// It ranges e.promotedAuthored rather than walking the concept registry
// asking about each concept in turn: the staged markers live in that map
// under a reserved key prefix, and the map holds promoted AUTHORED constructs
// only -- a small set, empty on an install that has never used the authoring
// path -- whereas the concept registry holds every concept the binary embeds.
//
// SCOPE-AWARE since memql#4040: a concept the read explicitly scoped in is
// staged but not withheld, so it is omitted here and no `!=` term is rendered
// for it. The filter runs through stagedConceptWithheld -- the same function
// admitStagedRow consults -- so the enumeration this drives and the per-row gate
// cannot disagree about what this read may see. The zero scope is the ordinary
// read and reproduces #3983's answer exactly.
//
// Returns nil when nothing is withheld, which is the answer on essentially
// every installation and the fast path every caller here keys off.
func (e *MemQLEngine) stagedConceptIds(scope StagedScope) []string {
	if e == nil {
		return nil
	}
	var ids []string
	e.promotedAuthored.Range(func(key, _ any) bool {
		name, ok := key.(string)
		if !ok {
			return true
		}
		conceptId, found := strings.CutPrefix(name, conceptDataStagedPrefix)
		if !found {
			return true
		}
		if conceptId = strings.TrimSpace(conceptId); conceptId != "" &&
			e.stagedConceptWithheld(scope, conceptId) {
			ids = append(ids, conceptId)
		}
		return true
	})
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	return ids
}

// stagedVisibilityPredicate renders and parses the conjunct that excludes
// every staged concept, or returns nil when nothing is staged.
//
// The rendered form is `row.concept!="<a>" && row.concept!="<b>"`. See the
// file header for why it is a chain of `!=` rather than the `out` spelling --
// the short version is that `out` hard-errors in the in-process re-check, and
// that re-check is what closes the loadLatestNodes swap.
//
// A parse failure is returned as an error rather than swallowed. A staged
// concept whose id will not render into a parseable predicate is a state this
// engine cannot enforce, and failing the read is the only honest answer:
// returning nil would silently publish the very rows the tier withholds.
func (e *MemQLEngine) stagedVisibilityPredicate(scope StagedScope) (ExpressionNode, error) {
	ids := e.stagedConceptIds(scope)
	if len(ids) == 0 {
		return nil, nil
	}
	signature := strings.Join(ids, "\n")
	if cached, ok := stagedPredicateCache.Load(signature); ok {
		return cloneStagedPredicate(cached.(ExpressionNode)), nil
	}

	terms := make([]string, 0, len(ids))
	for _, id := range ids {
		// A concept id carrying a quote or a backslash would escape the
		// literal. Ids are `v1:<ns>:<name>` and cannot, but the predicate is a
		// security boundary, so refuse rather than render something clever.
		if strings.ContainsAny(id, "\"\\") {
			return nil, fmt.Errorf(
				"staged-data: concept id %q contains a quote or backslash and cannot be rendered "+
					"into a visibility predicate; refusing the read rather than emitting a "+
					"predicate that would not mean what it says", id)
		}
		terms = append(terms, fmt.Sprintf("%s.concept!=%q", rowIntrinsicNamespace, id))
	}
	rendered := strings.Join(terms, " && ")

	parsed, err := parseViaLangparser(rendered, false)
	if err != nil {
		return nil, fmt.Errorf("staged-data: predicate %q does not parse: %w", rendered, err)
	}
	if parsed.Root == nil {
		return nil, fmt.Errorf("staged-data: predicate %q parsed to nothing", rendered)
	}
	stagedPredicateCache.Store(signature, parsed.Root)
	return cloneStagedPredicate(parsed.Root), nil
}

// cloneStagedPredicate deep-copies the cached tree.
//
// rewriteFilterFieldRefs mutates the nodes it walks -- it is what turns this
// predicate's `row.concept` into the bare `concept` both the SQL compiler and
// the in-process evaluator consume -- so handing out the cached tree itself
// would let the first query corrupt the entry every later query is given. The
// row-authz cache has the same hazard and clones for the same reason; that one
// only ever caches a single comparison, while this tree is a conjunction, so
// the copy has to recurse.
func cloneStagedPredicate(expr ExpressionNode) ExpressionNode {
	switch node := expr.(type) {
	case *ComparisonExpression:
		if node == nil {
			return nil
		}
		copied := *node
		return &copied
	case *LogicalExpression:
		if node == nil {
			return nil
		}
		return &LogicalExpression{
			Op:    node.Op,
			Left:  cloneStagedPredicate(node.Left),
			Right: cloneStagedPredicate(node.Right),
		}
	default:
		return expr
	}
}

// enforceStagedDataOnPlan ANDs the staged-visibility conjunct into the plan's
// root.
//
// Precedence mirrors enforceRowAuthzOnPlan: the term is ANDed at the ROOT, so
// an author's `a || b` becomes `((a) || (b)) && (staged)` rather than binding
// to the last disjunct.
//
// Deliberately NOT gated on plan.BoundConcept -- see the file header.
//
// scope arrives as a PARAMETER (memql#4040), resolved once by executeWith from
// the request context and handed down -- the same route auth.CallOrigin
// (memql#2800) and the ambient envelope (memql#3024) already take into this
// ctx-free parse path. The zero scope is the ordinary read.
func (e *MemQLEngine) enforceStagedDataOnPlan(plan *QueryPlan, scope StagedScope) error {
	if e == nil || plan == nil || plan.Root == nil {
		return nil
	}
	predicate, err := e.stagedVisibilityPredicate(scope)
	if err != nil {
		return err
	}
	if predicate == nil {
		return nil
	}
	plan.Root = &LogicalExpression{Op: LogicalAnd, Left: plan.Root, Right: predicate}
	return nil
}

// admitStagedRow reports whether a row may be shown to a read that resolved
// `scope`. It is THE correctness mechanism -- see the file header.
//
// A row whose concept is blank is ADMITTED, matching the identical decision
// admitRowAuthzBuiltinResult records beside it: a row with no concept is by
// definition not a row of a concept somebody staged, and denying it here would
// give one row two answers depending on which seam it left through.
//
// The scope is a PARAMETER rather than something this function reads off a
// context, even though its callers have one. It runs per ROW, and resolving the
// scope per row would be both wasteful and a chance for two rows of one read to
// be decided differently. The filters below resolve once and pass it in.
func (e *MemQLEngine) admitStagedRow(scope StagedScope, node memorynodes.MemoryNode) bool {
	if e == nil {
		return true
	}
	return !e.stagedConceptWithheld(scope, node.Concept)
}

// filterStagedSet drops every withheld row from a fetched set. Returns the set
// unchanged when nothing was withheld, so the common case allocates nothing.
//
// Shaped after filterRowAuthzSet, and applied immediately after it, because
// the two answer different questions about the same row: that gate asks "may
// this caller see it", this one asks "is it visible to anyone yet".
//
// Takes a ctx since memql#4040, to resolve the read's staged scope through
// stagedScopeFor -- the SAME resolver executeWith uses to build the plan-time
// conjunct, which is what stops the pushdown and the gate disagreeing about
// what this read may see. filterRowAuthzSet beside it already takes one.
func (e *MemQLEngine) filterStagedSet(ctx context.Context, set map[string]memorynodes.MemoryNode) map[string]memorynodes.MemoryNode {
	if e == nil || len(set) == 0 {
		return set
	}
	scope := e.stagedScopeFor(ctx)
	var denied []string
	for key, node := range set {
		if !e.admitStagedRow(scope, node) {
			denied = append(denied, key)
		}
	}
	if len(denied) == 0 {
		return set
	}
	for _, key := range denied {
		delete(set, key)
	}
	return set
}

// filterStagedNodes drops every withheld row from a slice, leaving the slice
// untouched when none was withheld.
//
// Filters the SLICE rather than a map built from it, for the reason
// filterRowAuthzBuiltinNodes gives: gating the producer's own output means the
// gate cannot be routed around by a later change to how that output is keyed.
//
// Takes a ctx for the reason filterStagedSet does.
func (e *MemQLEngine) filterStagedNodes(ctx context.Context, nodes []memorynodes.MemoryNode) []memorynodes.MemoryNode {
	if e == nil || len(nodes) == 0 {
		return nodes
	}
	scope := e.stagedScopeFor(ctx)
	denied := 0
	for _, node := range nodes {
		if !e.admitStagedRow(scope, node) {
			denied++
		}
	}
	if denied == 0 {
		return nodes
	}
	admitted := make([]memorynodes.MemoryNode, 0, len(nodes)-denied)
	for _, node := range nodes {
		if e.admitStagedRow(scope, node) {
			admitted = append(admitted, node)
		}
	}
	return admitted
}
